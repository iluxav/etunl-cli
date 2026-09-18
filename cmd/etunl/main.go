package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/iluxav/etunl-cli/internal/client"
	"github.com/iluxav/etunl-cli/internal/config"
	"github.com/iluxav/etunl-cli/internal/connect"
	"github.com/iluxav/etunl-cli/internal/server"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:     "etunl",
		Short:   "Reverse proxy and network tunnel",
		Version: version,
	}

	root.AddCommand(initCmd())
	root.AddCommand(serverCmd())
	root.AddCommand(clientCmd())
	root.AddCommand(connectCmd())
	root.AddCommand(addCmd())
	root.AddCommand(removeCmd())
	root.AddCommand(listCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func initCmd() *cobra.Command {
	var (
		cfgPath    string
		serverAddr string
		mode       string
	)
	cmd := &cobra.Command{
		Use:   "init [token]",
		Short: "Initialize config with a secret token (generates one if not provided)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var token string
			if len(args) > 0 {
				token = args[0]
			} else {
				tokenBytes := make([]byte, 32)
				if _, err := rand.Read(tokenBytes); err != nil {
					return fmt.Errorf("generate token: %w", err)
				}
				token = hex.EncodeToString(tokenBytes)
			}

			if mode == "server" {
				if cfgPath == "" {
					cfgPath = config.DefaultServerConfigPath()
				}
				cfg := &config.ServerConfig{
					ListenHTTP:   ":80",
					TCPPortRange: "15000-15100",
					Token:        token,
				}
				if err := config.SaveServerConfig(cfgPath, cfg); err != nil {
					return err
				}
				fmt.Printf("Server config created at %s\n", cfgPath)
			} else {
				if cfgPath == "" {
					cfgPath = config.DefaultConfigPath()
				}
				if serverAddr == "" {
					serverAddr = "tunnel.yourdomain.com"
				}
				machineName, _ := os.Hostname()
				if machineName == "" {
					machineName = "client"
				}
				cfg := &config.ClientConfig{
					Server:      serverAddr,
					Token:       token,
					MachineName: machineName,
					Routes: []config.Route{
						{
							Name:   "admin",
							Type:   "http",
							Target: "localhost:8080",
						},
					},
				}
				if err := config.SaveClientConfig(cfgPath, cfg); err != nil {
					return err
				}
				fmt.Printf("Client config created at %s\n", cfgPath)
			}

			fmt.Printf("Token: %s\n", token)
			fmt.Println("\nUse the same token on both server and client.")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config path")
	cmd.Flags().StringVar(&serverAddr, "server", "", "tunnel server address (client mode)")
	cmd.Flags().StringVar(&mode, "mode", "client", "config mode: client or server")
	return cmd
}

func serverCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the tunnel server (on DO/public machine)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultServerConfigPath()
			}
			cfg, err := config.LoadServerConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			srv := server.New(cfg, cfgPath)
			return srv.Start()
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "server config path")
	return cmd
}

func clientCmd() *cobra.Command {
	var (
		cfgPath       string
		dashboardAddr string
	)
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Run the tunnel client (on local machine)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultConfigPath()
			}
			c, err := client.New(cfgPath, dashboardAddr)
			if err != nil {
				return fmt.Errorf("init client: %w", err)
			}
			return c.Start()
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "client config path")
	cmd.Flags().StringVar(&dashboardAddr, "dashboard", ":8080", "dashboard listen address (empty to disable)")
	return cmd
}

func connectCmd() *cobra.Command {
	var (
		serverAddr string
		token      string
		name       string
		localPort  int
		cfgPath    string
	)
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to a TCP service through the tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load defaults from config if not specified
			if serverAddr == "" || token == "" {
				if cfgPath == "" {
					cfgPath = config.DefaultConfigPath()
				}
				cfg, err := config.LoadClientConfig(cfgPath)
				if err == nil {
					if serverAddr == "" {
						serverAddr = cfg.Server
					}
					if token == "" {
						token = cfg.Token
					}
				}
			}
			if serverAddr == "" || token == "" || name == "" || localPort == 0 {
				return fmt.Errorf("--server, --token, --name, and --local-port are required")
			}
			return connect.Run(connect.Options{
				Server:    serverAddr,
				Token:     token,
				RouteName: name,
				LocalPort: localPort,
			})
		},
	}
	cmd.Flags().StringVar(&serverAddr, "server", "", "tunnel server address")
	cmd.Flags().StringVar(&token, "token", "", "auth token")
	cmd.Flags().StringVar(&name, "name", "", "route name to connect to")
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local port to listen on")
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config path for defaults")
	return cmd
}

func addCmd() *cobra.Command {
	var (
		cfgPath   string
		name      string
		routeType string
		target    string
		localPort int
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a route to the config",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultConfigPath()
			}
			cfg, err := config.LoadClientConfig(cfgPath)
			if err != nil {
				return err
			}
			route := config.Route{
				Name:      name,
				Type:      routeType,
				Target:    target,
				LocalPort: localPort,
			}
			if err := cfg.AddRoute(route); err != nil {
				return err
			}
			if err := config.SaveClientConfig(cfgPath, cfg); err != nil {
				return err
			}
			fmt.Printf("added route: %s (%s) → %s\n", name, routeType, target)
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config path")
	cmd.Flags().StringVar(&name, "name", "", "route name")
	cmd.Flags().StringVar(&routeType, "type", "http", "route type (http or tcp)")
	cmd.Flags().StringVar(&target, "target", "", "target address (e.g. localhost:3030)")
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local port for TCP routes")
	cmd.MarkFlagRequired("name")
	cmd.MarkFlagRequired("target")
	return cmd
}

func removeCmd() *cobra.Command {
	var (
		cfgPath string
		name    string
	)
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a route from the config",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultConfigPath()
			}
			cfg, err := config.LoadClientConfig(cfgPath)
			if err != nil {
				return err
			}
			if err := cfg.RemoveRoute(name); err != nil {
				return err
			}
			if err := config.SaveClientConfig(cfgPath, cfg); err != nil {
				return err
			}
			fmt.Printf("removed route: %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config path")
	cmd.Flags().StringVar(&name, "name", "", "route name to remove")
	cmd.MarkFlagRequired("name")
	return cmd
}

func listCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured routes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultConfigPath()
			}
			cfg, err := config.LoadClientConfig(cfgPath)
			if err != nil {
				return err
			}
			if len(cfg.Routes) == 0 {
				fmt.Println("no routes configured")
				return nil
			}
			fmt.Printf("%-15s %-6s %-25s %s\n", "NAME", "TYPE", "TARGET", "LOCAL PORT")
			for _, r := range cfg.Routes {
				lp := "-"
				if r.LocalPort > 0 {
					lp = fmt.Sprintf("%d", r.LocalPort)
				}
				fmt.Printf("%-15s %-6s %-25s %s\n", r.Name, r.Type, r.Target, lp)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config path")
	return cmd
}
