package server

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iluxav/etunl-cli/internal/config"
	"github.com/iluxav/etunl-cli/internal/tunnel"
)

type tcpListener struct {
	port     int
	listener net.Listener
	cancel   context.CancelFunc
}

type clientEntry struct {
	machineName string
	conn        *tunnel.Conn
	routes      []tunnel.RouteInfo
	remoteAddr  string
	connectedAt time.Time
}

type Server struct {
	cfg      *config.ServerConfig
	cfgPath  string
	upgrader websocket.Upgrader
	metrics  *Registry

	mu           sync.RWMutex
	clients      map[string]*clientEntry
	tcpListeners map[string]*tcpListener // route name → listener
	portStart    int
	portEnd      int
}

func New(cfg *config.ServerConfig, cfgPath string) *Server {
	portStart, portEnd, err := cfg.ParseTCPPortRange()
	if err != nil {
		log.Printf("WARNING: invalid tcp_port_range: %v, TCP tunneling disabled", err)
		portStart, portEnd = 0, 0
	}
	return &Server{
		cfg:     cfg,
		cfgPath: cfgPath,
		metrics: NewRegistry(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients:      make(map[string]*clientEntry),
		tcpListeners: make(map[string]*tcpListener),
		portStart:    portStart,
		portEnd:      portEnd,
	}
}

func (s *Server) Start() error {
	log.Printf("server listening on %s (HTTP), TCP port range %d-%d", s.cfg.ListenHTTP, s.portStart, s.portEnd)
	return http.ListenAndServe(s.cfg.ListenHTTP, s)
}

// allocatePort returns a deterministic port for the given route name.
// Caller must hold s.mu.
func (s *Server) allocatePort(routeName string) (int, error) {
	if s.portStart == 0 && s.portEnd == 0 {
		return 0, fmt.Errorf("TCP port range not configured")
	}
	rangeSize := s.portEnd - s.portStart
	h := fnv.New32a()
	h.Write([]byte(routeName))
	port := s.portStart + int(h.Sum32())%rangeSize

	// Check if already in use by another route
	for name, tl := range s.tcpListeners {
		if tl.port == port && name != routeName {
			// Linear scan for next free port
			for p := s.portStart; p < s.portEnd; p++ {
				taken := false
				for _, tl2 := range s.tcpListeners {
					if tl2.port == p {
						taken = true
						break
					}
				}
				if !taken {
					return p, nil
				}
			}
			return 0, fmt.Errorf("no free TCP ports in range %d-%d", s.portStart, s.portEnd)
		}
	}
	return port, nil
}

// ServeHTTP handles all incoming HTTP requests.
// Requests to /tunnel are the tunnel WebSocket endpoint.
// Requests with the admin subdomain go to the admin UI.
// All other requests are proxied based on subdomain.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/tunnel":
		s.handleTunnel(w, r)
		return
	case "/health":
		s.handleHealth(w, r)
		return
	}

	if sub := extractSubdomain(r.Host); sub != "" && sub == s.cfg.AdminSubdomain {
		s.handleAdmin(w, r)
		return
	}

	s.handleHTTPProxy(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	machines := make([]map[string]any, 0, len(s.clients))
	totalRoutes := 0
	for _, e := range s.clients {
		totalRoutes += len(e.routes)
		machines = append(machines, map[string]any{
			"machine_name": e.machineName,
			"routes":       redactRouteInfos(e.routes),
		})
	}
	tcpPorts := make(map[string]int, len(s.tcpListeners))
	for name, tl := range s.tcpListeners {
		tcpPorts[name] = tl.port
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"machines":  machines,
		"routes":    totalRoutes,
		"tcp_ports": tcpPorts,
	})
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	expected := "Bearer " + s.cfg.Token
	s.mu.RUnlock()
	token := r.Header.Get("Authorization")
	if token != expected {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	machineName := r.Header.Get("X-Machine-Name")

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}

	if machineName == "" {
		// anonymous connection (e.g. etunl connect): serve but don't register.
		log.Printf("anonymous tunnel connection from %s", r.RemoteAddr)
		conn := tunnel.NewConn(ws, nil)
		conn.StartPing()
		err := conn.ReadLoop()
		log.Printf("anonymous tunnel disconnected: %v", err)
		return
	}

	s.mu.Lock()
	if old, ok := s.clients[machineName]; ok {
		log.Printf("machine %q reconnecting; closing previous connection", machineName)
		old.conn.Close()
	}
	conn := tunnel.NewConn(ws, nil)
	entry := &clientEntry{
		machineName: machineName,
		conn:        conn,
		remoteAddr:  r.RemoteAddr,
		connectedAt: time.Now(),
	}
	s.clients[machineName] = entry
	s.mu.Unlock()

	log.Printf("tunnel client %q connected from %s", machineName, r.RemoteAddr)
	conn.StartPing()

	conn.Mux().SetRouteSyncHandler(func(payload []byte) {
		var routes []tunnel.RouteInfo
		if err := json.Unmarshal(payload, &routes); err != nil {
			log.Printf("invalid route sync from %q: %v", machineName, err)
			return
		}

		s.mu.Lock()
		accepted := make([]tunnel.RouteInfo, 0, len(routes))
		for _, rt := range routes {
			if owner := s.findOwnerLocked(rt.Name); owner != "" && owner != machineName {
				log.Printf("route %q from %q rejected: already owned by %q", rt.Name, machineName, owner)
				continue
			}
			accepted = append(accepted, rt)
		}
		entry.routes = accepted
		s.mu.Unlock()

		log.Printf("routes updated for %q: %d accepted / %d sent", machineName, len(accepted), len(routes))
		for _, r := range accepted {
			log.Printf("  - %s (%s)", r.Name, r.Type)
		}
		s.syncTCPListeners()
	})

	err = conn.ReadLoop()
	log.Printf("tunnel client %q disconnected: %v", machineName, err)

	s.mu.Lock()
	if s.clients[machineName] == entry {
		delete(s.clients, machineName)
	}
	s.mu.Unlock()
	s.syncTCPListeners()
}

// findRoute locates a route by subdomain across all connected machines
// and returns the route info, owning machine name, and tunnel conn.
func (s *Server) findRoute(name string) (*tunnel.RouteInfo, string, *tunnel.Conn) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.clients {
		for i := range entry.routes {
			if entry.routes[i].Name == name {
				rt := entry.routes[i]
				return &rt, entry.machineName, entry.conn
			}
		}
	}
	return nil, "", nil
}

// findOwnerLocked returns the machine name that owns a given route, or "".
// Caller must hold s.mu.
func (s *Server) findOwnerLocked(routeName string) string {
	for name, entry := range s.clients {
		for _, r := range entry.routes {
			if r.Name == routeName {
				return name
			}
		}
	}
	return ""
}
