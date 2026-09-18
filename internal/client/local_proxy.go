package client

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/iluxav/etunl-cli/internal/config"
)

type LocalProxy struct {
	mu     sync.RWMutex
	routes []config.Route
}

func NewLocalProxy() *LocalProxy {
	return &LocalProxy{}
}

func (lp *LocalProxy) UpdateRoutes(routes []config.Route) {
	lp.mu.Lock()
	lp.routes = routes
	lp.mu.Unlock()
	log.Printf("local proxy updated: %d routes", len(routes))
}

func (lp *LocalProxy) Start() {
	// Start HTTP proxy on :80 for subdomain routing
	go lp.startHTTP()

	// Start TCP proxies for each TCP route
	lp.startTCPProxies()
}

func (lp *LocalProxy) startHTTP() {
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subdomain := extractLocalSubdomain(r.Host)
		if subdomain == "" {
			http.Error(w, "no subdomain", http.StatusBadRequest)
			return
		}

		lp.mu.RLock()
		var route *config.Route
		for i := range lp.routes {
			if lp.routes[i].Name == subdomain && lp.routes[i].Type == "http" {
				route = &lp.routes[i]
				break
			}
		}
		lp.mu.RUnlock()

		if route == nil {
			http.Error(w, "route not found: "+subdomain, http.StatusNotFound)
			return
		}

		if route.Auth != nil {
			wireAuth := authToWire(route.Auth)
			// Session (cookie) auth is enforced by the public server only;
			// the local proxy is reachable from this machine and is left open.
			if !wireAuth.HasSession() && !wireAuth.Check(r) {
				wireAuth.WriteUnauthorized(w)
				return
			}
		}

		target, err := url.Parse("http://" + route.Target)
		if err != nil {
			http.Error(w, "invalid target", http.StatusInternalServerError)
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ServeHTTP(w, r)
	})

	log.Printf("local HTTP proxy on :80")
	if err := http.ListenAndServe(":80", mux); err != nil {
		log.Printf("local HTTP proxy failed: %v (try running with sudo or use a higher port)", err)
	}
}

func (lp *LocalProxy) startTCPProxies() {
	lp.mu.RLock()
	routes := make([]config.Route, len(lp.routes))
	copy(routes, lp.routes)
	lp.mu.RUnlock()

	for _, r := range routes {
		if r.Type == "tcp" && r.LocalPort > 0 {
			go lp.startTCPProxy(r)
		}
	}
}

func (lp *LocalProxy) startTCPProxy(route config.Route) {
	addr := fmt.Sprintf(":%d", route.LocalPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("local TCP proxy for %s on %s failed: %v", route.Name, addr, err)
		return
	}
	defer ln.Close()

	log.Printf("local TCP proxy: %s on %s → %s", route.Name, addr, route.Target)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("TCP accept error for %s: %v", route.Name, err)
			continue
		}
		go func() {
			defer conn.Close()
			target, err := net.Dial("tcp", route.Target)
			if err != nil {
				log.Printf("failed to dial %s for route %s: %v", route.Target, route.Name, err)
				return
			}
			defer target.Close()

			done := make(chan struct{})
			go func() {
				io.Copy(target, conn)
				close(done)
			}()
			io.Copy(conn, target)
			<-done
		}()
	}
}

func extractLocalSubdomain(host string) string {
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	parts := strings.SplitN(host, ".", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}
