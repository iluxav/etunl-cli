package client

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/iluxav/etunl-cli/internal/config"
)

//go:embed web/index.html
var webFS embed.FS

// routeView is the redacted shape of a route sent to the dashboard. The
// auth secret value is never serialised; only the "scheme" label is.
type routeView struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Target    string `json:"target"`
	LocalPort int    `json:"local_port,omitempty"`
	Auth      string `json:"auth,omitempty"` // "" | "bearer" | "session" | "<header-name>"
}

func redactRoutes(routes []config.Route) []routeView {
	out := make([]routeView, len(routes))
	for i, r := range routes {
		out[i] = routeView{Name: r.Name, Type: r.Type, Target: r.Target, LocalPort: r.LocalPort}
		if r.Auth != nil {
			switch {
			case r.Auth.Bearer != "":
				out[i].Auth = "bearer"
			case len(r.Auth.Users) > 0:
				out[i].Auth = "session"
			default:
				out[i].Auth = r.Auth.Header
			}
		}
	}
	return out
}

func (c *Client) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		cfg := c.watcher.Config()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="etunl"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (c *Client) startDashboard(addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", c.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		data, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html")
		w.Write(data)
	}))

	mux.HandleFunc("/api/status", c.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		cfg := c.watcher.Config()
		connected := c.conn != nil
		machineName := cfg.MachineName
		if machineName == "" {
			machineName, _ = os.Hostname()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tunnel_connected": connected,
			"routes":           len(cfg.Routes),
			"server":           cfg.Server,
			"machine_name":     machineName,
		})
	}))

	mux.HandleFunc("/api/routes", c.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg := c.watcher.Config()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(redactRoutes(cfg.Routes))

		case http.MethodPost:
			var route config.Route
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(body, &route); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if route.Name == "" || route.Target == "" {
				http.Error(w, "name and target are required", http.StatusBadRequest)
				return
			}
			if route.Type == "" {
				route.Type = "http"
			}
			if route.Auth != nil {
				if route.Type != "http" {
					http.Error(w, "auth is only supported on http routes", http.StatusBadRequest)
					return
				}
				if err := route.Auth.Validate(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}

			cfg := c.watcher.Config()
			if err := cfg.AddRoute(route); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			if err := config.SaveClientConfig(c.configPath, cfg); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/routes/", c.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/routes/")
		if name == "" {
			http.Error(w, "route name required", http.StatusBadRequest)
			return
		}

		cfg := c.watcher.Config()
		if err := cfg.RemoveRoute(name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err := config.SaveClientConfig(c.configPath, cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	log.Printf("dashboard on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("dashboard failed: %v", err)
	}
}
