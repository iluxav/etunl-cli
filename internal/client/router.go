package client

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/iluxav/etunl-cli/internal/config"
	"github.com/iluxav/etunl-cli/internal/tunnel"
)

func (c *Client) handleHTTPStream(s *tunnel.Stream, route *config.Route) {
	defer c.conn.Mux().CloseStream(s.ID)

	// Bridge stream data channel into an io.Reader via pipe
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			select {
			case data, ok := <-s.DataCh:
				if !ok {
					return
				}
				if _, err := pw.Write(data); err != nil {
					return
				}
			case <-s.Done:
				return
			}
		}
	}()

	br := bufio.NewReader(pr)
	req, err := http.ReadRequest(br)
	if err != nil {
		log.Printf("failed to read HTTP request from stream: %v", err)
		return
	}

	if isWebSocketUpgrade(req) {
		c.forwardWebSocket(s, req, route, pr, br)
		return
	}

	c.forwardHTTP(s, req, route)
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

func (c *Client) forwardWebSocket(s *tunnel.Stream, req *http.Request, route *config.Route, pr *io.PipeReader, br *bufio.Reader) {
	targetConn, err := net.Dial("tcp", route.Target)
	if err != nil {
		log.Printf("ws: failed to dial %s for %s: %v", route.Target, route.Name, err)
		return
	}
	defer targetConn.Close()

	if err := req.Write(targetConn); err != nil {
		log.Printf("ws: write request to %s: %v", route.Target, err)
		return
	}

	done := make(chan struct{})

	// target → tunnel
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := targetConn.Read(buf)
			if n > 0 {
				if sendErr := c.conn.Mux().SendData(s.ID, buf[:n]); sendErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// When the target side ends, unblock the copy below by closing the pipe.
	go func() {
		<-done
		pr.Close()
	}()

	// tunnel (via br to keep any post-headers bytes already buffered) → target
	io.Copy(targetConn, br)
}

func (c *Client) forwardHTTP(s *tunnel.Stream, req *http.Request, route *config.Route) {
	targetConn, err := net.Dial("tcp", route.Target)
	if err != nil {
		log.Printf("failed to dial %s: %v", route.Target, err)
		resp := &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString("Bad Gateway: target unreachable")),
		}
		var buf bytes.Buffer
		resp.Write(&buf)
		c.conn.Mux().SendData(s.ID, buf.Bytes())
		return
	}
	defer targetConn.Close()

	if err := req.Write(targetConn); err != nil {
		log.Printf("failed to write request to %s: %v", route.Target, err)
		return
	}

	// Read response from target and send back through tunnel
	buf := make([]byte, 32*1024)
	for {
		n, err := targetConn.Read(buf)
		if n > 0 {
			if sendErr := c.conn.Mux().SendData(s.ID, buf[:n]); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) handleTCPStream(s *tunnel.Stream, route *config.Route) {
	defer c.conn.Mux().CloseStream(s.ID)

	targetConn, err := net.Dial("tcp", route.Target)
	if err != nil {
		log.Printf("failed to dial TCP target %s: %v", route.Target, err)
		return
	}
	defer targetConn.Close()

	done := make(chan struct{})

	// Target → tunnel
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := targetConn.Read(buf)
			if n > 0 {
				if sendErr := c.conn.Mux().SendData(s.ID, buf[:n]); sendErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Tunnel → target
	for {
		select {
		case data, ok := <-s.DataCh:
			if !ok {
				return
			}
			if _, err := targetConn.Write(data); err != nil {
				return
			}
		case <-s.Done:
			return
		case <-done:
			return
		}
	}
}
