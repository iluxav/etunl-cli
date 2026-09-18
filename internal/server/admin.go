package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/iluxav/etunl-cli/internal/config"
	"github.com/iluxav/etunl-cli/internal/tunnel"
)

//go:embed web/admin.html
var adminWebFS embed.FS

// routeView is the admin-facing form of a RouteInfo. The auth secret
// itself is never serialised; only a "scheme" label is exposed so the
// dashboard can show whether protection is on and what kind.
type routeView struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	LocalPort int    `json:"local_port,omitempty"`
	Auth      string `json:"auth,omitempty"` // "" | "bearer" | "session" | "<header-name>"
}

func redactRouteInfos(routes []tunnel.RouteInfo) []routeView {
	out := make([]routeView, len(routes))
	for i, r := range routes {
		out[i] = routeView{Name: r.Name, Type: r.Type, LocalPort: r.LocalPort, Auth: r.Auth.Scheme()}
	}
	return out
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	user := s.cfg.AdminUser
	pass := s.cfg.AdminPassword
	s.mu.RUnlock()
	setupRequired := user == "" || pass == ""

	// The HTML page is always served — the JS picks setup vs. dashboard based on /api/status.
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		data, _ := adminWebFS.ReadFile("web/admin.html")
		w.Header().Set("Content-Type", "text/html")
		w.Write(data)
		return
	}

	// /api/setup is open while admin is unconfigured; it self-locks once credentials exist.
	if r.URL.Path == "/api/setup" {
		if !setupRequired {
			http.Error(w, "admin already configured", http.StatusConflict)
			return
		}
		s.adminSetup(w, r)
		return
	}

	// /api/status reports setup_required without auth so the page can show the setup form.
	if r.URL.Path == "/api/status" && setupRequired {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"setup_required": true})
		return
	}

	if setupRequired {
		http.Error(w, "admin not yet configured — visit / to set up", http.StatusServiceUnavailable)
		return
	}

	gotUser, gotPass, ok := r.BasicAuth()
	if !ok ||
		subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) != 1 ||
		subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="etunl admin"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/api/status":
		s.adminStatus(w, r)
	case "/api/metrics":
		s.adminMetrics(w, r)
	case "/api/rotate-token":
		s.adminRotateToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"routes": s.metrics.Snapshot(),
	})
}

func (s *Server) adminSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.cfg.AdminUser != "" && s.cfg.AdminPassword != "" {
		s.mu.Unlock()
		http.Error(w, "admin already configured", http.StatusConflict)
		return
	}
	s.cfg.AdminUser = req.User
	s.cfg.AdminPassword = req.Password
	cfgCopy := *s.cfg
	cfgPath := s.cfgPath
	s.mu.Unlock()

	if err := config.SaveServerConfig(cfgPath, &cfgCopy); err != nil {
		// Roll back so a retry can succeed.
		s.mu.Lock()
		s.cfg.AdminUser = ""
		s.cfg.AdminPassword = ""
		s.mu.Unlock()
		log.Printf("admin: failed to persist setup: %v", err)
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("admin: initial credentials configured for user %q", req.User)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	machines := make([]map[string]any, 0, len(s.clients))
	for _, e := range s.clients {
		machines = append(machines, map[string]any{
			"machine_name": e.machineName,
			"remote_addr":  e.remoteAddr,
			"connected_at": e.connectedAt.UTC().Format(time.RFC3339),
			"routes":       redactRouteInfos(e.routes),
		})
	}
	tcpPorts := make(map[string]int, len(s.tcpListeners))
	for name, tl := range s.tcpListeners {
		tcpPorts[name] = tl.port
	}
	token := s.cfg.Token
	adminSub := s.cfg.AdminSubdomain
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"machines":      machines,
		"tcp_ports":     tcpPorts,
		"token":         token,
		"public_domain": derivePublicDomain(r.Host, adminSub),
	})
}

func (s *Server) adminRotateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	newToken, err := config.GenerateToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.cfg.Token = newToken
	cfgCopy := *s.cfg
	cfgPath := s.cfgPath
	s.mu.Unlock()

	if err := config.SaveServerConfig(cfgPath, &cfgCopy); err != nil {
		log.Printf("admin: failed to persist new token: %v", err)
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("admin: tunnel token rotated")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"token": newToken})
}

func derivePublicDomain(host, adminSub string) string {
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	prefix := adminSub + "."
	if strings.HasPrefix(host, prefix) {
		return host[len(prefix):]
	}
	return host
}
