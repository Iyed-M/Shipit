// Package caddy keeps Caddy's routes in step with Shipit's database.
//
// Shipit owns Caddy's whole configuration: each change renders it from the
// database and loads it atomically, so the two can't drift apart and a restart
// of either side recovers by syncing again.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Iyed-M/Shipit/internal/store"
)

// AppPort is the port every app listens on. Each app has its own container, so
// they can't collide. Shipit passes it to the app as $PORT.
const AppPort = 3000

// Config describes the routes that don't come from the database.
type Config struct {
	AdminSocket string
	// Domain is the dashboard host. Projects live at <project>.<Domain>.
	Domain            string
	DashboardUpstream string
	StaticHost        string
	StaticRoot        string
}

// App is one project's route. An empty Upstream means nothing is live yet.
type App struct {
	Host     string
	Upstream string
}

// RouteLister provides the routes to publish.
type RouteLister interface {
	Routes(ctx context.Context) ([]store.Route, error)
}

// Router pushes the current routes to Caddy's admin API.
type Router struct {
	cfg    Config
	routes RouteLister
	http   *http.Client
	mu     sync.Mutex
}

// NewRouter returns a Router that reaches Caddy over its admin socket.
func NewRouter(cfg Config, routes RouteLister) *Router {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", cfg.AdminSocket)
		},
	}
	return &Router{
		cfg:    cfg,
		routes: routes,
		http:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

// Sync replaces Caddy's configuration with one rendered from the database.
func (r *Router) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	routes, err := r.routes.Routes(ctx)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	apps := make([]App, 0, len(routes))
	for _, route := range routes {
		app := App{Host: route.Project + "." + r.cfg.Domain}
		if route.Container != "" {
			app.Upstream = fmt.Sprintf("%s:%d", route.Container, AppPort)
		}
		apps = append(apps, app)
	}

	payload, err := json.Marshal(Render(r.cfg, apps))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/load", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("load caddy config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("load caddy config: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// WaitReady syncs until Caddy accepts the configuration or ctx ends.
func (r *Router) WaitReady(ctx context.Context) error {
	for {
		err := r.Sync(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("caddy not ready: %w", err)
		case <-time.After(time.Second):
		}
	}
}

type object = map[string]any

// Render builds Caddy's full JSON configuration. The admin block must be part
// of it: loading a config without one would move the admin API off the socket
// and lock Shipit out.
func Render(cfg Config, apps []App) object {
	routes := []object{
		route(cfg.Domain, reverseProxy(cfg.DashboardUpstream)),
	}
	for _, app := range apps {
		if app.Upstream == "" {
			routes = append(routes, route(app.Host, object{
				"handler":     "static_response",
				"status_code": http.StatusServiceUnavailable,
				"body":        "No deploy is live yet.\n",
			}))
			continue
		}
		routes = append(routes, route(app.Host, reverseProxy(app.Upstream)))
	}
	if cfg.StaticHost != "" {
		routes = append(routes, route(cfg.StaticHost, object{
			"handler": "file_server",
			"root":    cfg.StaticRoot,
		}))
	}

	return object{
		"admin": object{"listen": "unix/" + cfg.AdminSocket},
		"apps": object{
			"http": object{
				"servers": object{
					"shipit": object{
						"listen": []string{":443"},
						"routes": routes,
					},
				},
			},
		},
	}
}

func route(host string, handler object) object {
	return object{
		"match":    []object{{"host": []string{host}}},
		"handle":   []object{handler},
		"terminal": true,
	}
}

func reverseProxy(upstream string) object {
	return object{
		"handler":   "reverse_proxy",
		"upstreams": []object{{"dial": upstream}},
	}
}
