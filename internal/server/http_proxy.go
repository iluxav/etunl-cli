package server

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/iluxav/etunl-cli/internal/tunnel"
)

func (s *Server) handleHTTPProxy(w http.ResponseWriter, r *http.Request) {
	subdomain := extractSubdomain(r.Host)
	if subdomain == "" {
		http.Error(w, "no subdomain specified", http.StatusBadRequest)
		return
	}

	route, machine, conn := s.findRoute(subdomain)
	if route == nil {
		http.Error(w, "route not found: "+subdomain, http.StatusNotFound)
		return
	}
	if route.Type != "http" {
		http.Error(w, "route is not HTTP: "+subdomain, http.StatusBadRequest)
		return
	}
	if conn == nil {
		http.Error(w, "tunnel not connected", http.StatusBadGateway)
		return
	}
	if route.Auth.HasSession() {
		if s.handleSessionAuth(w, r, route) {
			return
		}
	} else if !route.Auth.Check(r) {
		route.Auth.WriteUnauthorized(w)
		return
	}

	if isWebSocketUpgrade(r) {
		s.handleWebSocketProxy(w, r, conn, machine, subdomain)
		return
	}

	requestStart := time.Now()

	stream, err := conn.Mux().OpenStream(subdomain)
	if err != nil {
		http.Error(w, "failed to open tunnel stream", http.StatusBadGateway)
		return
	}
	defer conn.Mux().CloseStream(stream.ID)

	// Serialize the HTTP request and send through tunnel
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		if err := r.Write(pw); err != nil {
			return
		}
	}()

	// Send request body through tunnel
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				if sendErr := conn.Mux().SendData(stream.ID, buf[:n]); sendErr != nil {
					return
				}
				s.metrics.RecordBytesIn(machine, subdomain, "http", n)
			}
			if err != nil {
				return
			}
		}
	}()

	// Bridge stream data channel into an io.Reader via pipe
	respPR, respPW := io.Pipe()
	go func() {
		defer respPW.Close()
		for {
			select {
			case data, ok := <-stream.DataCh:
				if !ok {
					return
				}
				if _, err := respPW.Write(data); err != nil {
					return
				}
				s.metrics.RecordBytesOut(machine, subdomain, "http", len(data))
			case <-stream.Done:
				return
			}
		}
	}()

	// Parse HTTP response from the pipe reader
	resp, err := http.ReadResponse(bufio.NewReader(respPR), r)
	if err != nil {
		http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	s.metrics.RecordHTTPRequest(machine, subdomain, time.Since(requestStart))
}

func extractSubdomain(host string) string {
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	parts := strings.SplitN(host, ".", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleWebSocketProxy(w http.ResponseWriter, r *http.Request, conn *tunnel.Conn, machine, subdomain string) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket: response writer not hijackable", http.StatusInternalServerError)
		return
	}

	var reqBuf bytes.Buffer
	if err := r.Write(&reqBuf); err != nil {
		http.Error(w, "websocket: serialize request", http.StatusInternalServerError)
		return
	}

	stream, err := conn.Mux().OpenStream(subdomain)
	if err != nil {
		http.Error(w, "failed to open tunnel stream", http.StatusBadGateway)
		return
	}
	defer conn.Mux().CloseStream(stream.ID)

	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("websocket hijack failed: %v", err)
		return
	}
	defer clientConn.Close()

	s.metrics.OpenConn(machine, subdomain, "ws")
	defer s.metrics.CloseConn(machine, subdomain, "ws")

	if err := conn.Mux().SendData(stream.ID, reqBuf.Bytes()); err != nil {
		log.Printf("websocket: send request to tunnel: %v", err)
		return
	}
	s.metrics.RecordBytesIn(machine, subdomain, "ws", reqBuf.Len())

	done := make(chan struct{})

	// browser → tunnel
	go func() {
		defer close(done)
		// Forward any bytes the http server already buffered before hijack.
		if n := bufrw.Reader.Buffered(); n > 0 {
			buf := make([]byte, n)
			if _, err := io.ReadFull(bufrw.Reader, buf); err == nil {
				if sendErr := conn.Mux().SendData(stream.ID, buf); sendErr != nil {
					return
				}
				s.metrics.RecordBytesIn(machine, subdomain, "ws", n)
			}
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				if sendErr := conn.Mux().SendData(stream.ID, buf[:n]); sendErr != nil {
					return
				}
				s.metrics.RecordBytesIn(machine, subdomain, "ws", n)
			}
			if err != nil {
				return
			}
		}
	}()

	// tunnel → browser
	for {
		select {
		case data, ok := <-stream.DataCh:
			if !ok {
				return
			}
			if _, err := clientConn.Write(data); err != nil {
				return
			}
			s.metrics.RecordBytesOut(machine, subdomain, "ws", len(data))
		case <-stream.Done:
			return
		case <-done:
			return
		}
	}
}
