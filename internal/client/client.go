package client

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/iluxav/etunl-cli/internal/config"
	"github.com/iluxav/etunl-cli/internal/tunnel"
	"github.com/gorilla/websocket"
)

type Client struct {
	watcher       *config.Watcher
	configPath    string
	conn          *tunnel.Conn
	localProxy    *LocalProxy
	dashboardAddr string
}

func New(configPath string, dashboardAddr string) (*Client, error) {
	c := &Client{configPath: configPath, dashboardAddr: dashboardAddr}

	watcher, err := config.NewWatcher(configPath, c.onConfigChange)
	if err != nil {
		return nil, fmt.Errorf("config watcher: %w", err)
	}
	c.watcher = watcher
	c.localProxy = NewLocalProxy()

	return c, nil
}

func (c *Client) Start() error {
	cfg := c.watcher.Config()

	if err := c.watcher.Start(); err != nil {
		return fmt.Errorf("start config watcher: %w", err)
	}

	// Start local proxy for same-machine access
	c.localProxy.UpdateRoutes(cfg.Routes)
	go c.localProxy.Start()

	// Start dashboard
	if c.dashboardAddr != "" {
		go c.startDashboard(c.dashboardAddr)
	}

	// Connect to tunnel server with reconnect
	c.connectLoop(cfg)
	return nil
}

func (c *Client) connectLoop(cfg *config.ClientConfig) {
	attempt := 0
	for {
		err := c.connect(cfg)
		if err != nil {
			attempt++
			delay := backoff(attempt)
			log.Printf("tunnel disconnected: %v (reconnecting in %v)", err, delay)
			time.Sleep(delay)
			// Refresh config in case server address changed
			cfg = c.watcher.Config()
		}
	}
}

func (c *Client) connect(cfg *config.ClientConfig) error {
	machineName := cfg.MachineName
	if machineName == "" {
		machineName, _ = os.Hostname()
	}
	if machineName == "" {
		return fmt.Errorf("machine_name is empty and hostname is unavailable; set machine_name in config")
	}

	url := fmt.Sprintf("ws://%s/tunnel", cfg.Server)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cfg.Token)
	header.Set("X-Machine-Name", machineName)

	log.Printf("connecting to %s as %q", url, machineName)

	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	conn := tunnel.NewConn(ws, c.handleStream)
	c.conn = conn
	conn.StartPing()

	// Send initial route sync
	routes := configToRouteInfo(cfg.Routes)
	if err := conn.Mux().SendRouteSync(routes); err != nil {
		conn.Close()
		return fmt.Errorf("route sync: %w", err)
	}

	log.Printf("tunnel connected, %d routes synced", len(routes))

	// Block until connection drops
	return conn.ReadLoop()
}

func (c *Client) handleStream(s *tunnel.Stream) {
	cfg := c.watcher.Config()
	route := cfg.FindRoute(s.Route)
	if route == nil {
		log.Printf("stream for unknown route: %s", s.Route)
		if c.conn != nil {
			c.conn.Mux().CloseStream(s.ID)
		}
		return
	}

	if route.Type == "http" {
		c.handleHTTPStream(s, route)
	} else {
		c.handleTCPStream(s, route)
	}
}

func (c *Client) onConfigChange(cfg *config.ClientConfig) {
	c.localProxy.UpdateRoutes(cfg.Routes)

	if c.conn != nil {
		routes := configToRouteInfo(cfg.Routes)
		if err := c.conn.Mux().SendRouteSync(routes); err != nil {
			log.Printf("failed to sync routes: %v", err)
		} else {
			log.Printf("routes synced: %d routes", len(routes))
		}
	}
}

func configToRouteInfo(routes []config.Route) []tunnel.RouteInfo {
	infos := make([]tunnel.RouteInfo, len(routes))
	for i, r := range routes {
		infos[i] = tunnel.RouteInfo{
			Name:      r.Name,
			Type:      r.Type,
			LocalPort: r.LocalPort,
			Auth:      authToWire(r.Auth),
		}
	}
	return infos
}

func authToWire(a *config.Auth) *tunnel.RouteAuth {
	if a == nil {
		return nil
	}
	out := &tunnel.RouteAuth{Bearer: a.Bearer, Header: a.Header, Value: a.Value}
	if len(a.Users) > 0 {
		out.Users = make([]tunnel.RouteAuthUser, len(a.Users))
		for i, u := range a.Users {
			out.Users[i] = tunnel.RouteAuthUser{User: u.User, Password: u.Password}
		}
	}
	return out
}

func backoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
