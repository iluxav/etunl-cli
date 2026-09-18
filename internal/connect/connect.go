package connect

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/iluxav/etunl-cli/internal/tunnel"
	"github.com/gorilla/websocket"
)

type Options struct {
	Server    string
	Token     string
	RouteName string
	LocalPort int
}

func Run(opts Options) error {
	addr := fmt.Sprintf(":%d", opts.LocalPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer ln.Close()

	log.Printf("listening on %s, tunneling to route %q via %s", addr, opts.RouteName, opts.Server)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleConn(conn, opts)
	}
}

func handleConn(c net.Conn, opts Options) {
	defer c.Close()

	// Each incoming connection gets its own WebSocket to the server
	url := fmt.Sprintf("ws://%s/tunnel", opts.Server)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+opts.Token)

	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		log.Printf("failed to connect to tunnel server: %v", err)
		return
	}

	tConn := tunnel.NewConn(ws, nil)
	defer tConn.Close()

	stream, err := tConn.Mux().OpenStream(opts.RouteName)
	if err != nil {
		log.Printf("failed to open stream for %s: %v", opts.RouteName, err)
		return
	}
	defer tConn.Mux().CloseStream(stream.ID)

	// Start reading tunnel frames in background
	go tConn.ReadLoop()

	done := make(chan struct{})

	// Local conn → tunnel
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if sendErr := tConn.Mux().SendData(stream.ID, buf[:n]); sendErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("read error: %v", err)
				}
				return
			}
		}
	}()

	// Tunnel → local conn
	for {
		select {
		case data, ok := <-stream.DataCh:
			if !ok {
				return
			}
			if _, err := c.Write(data); err != nil {
				return
			}
		case <-stream.Done:
			return
		case <-done:
			return
		}
	}
}
