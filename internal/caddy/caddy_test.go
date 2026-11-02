package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Iyed-M/Shipit/internal/store"
)

type routes []store.Route

func (r routes) Routes(context.Context) ([]store.Route, error) { return r, nil }

func TestRenderKeepsAdminOnSocket(t *testing.T) {
	config := Render(Config{AdminSocket: "/run/caddy/admin.sock"}, nil)

	admin := config["admin"].(object)
	if admin["listen"] != "unix//run/caddy/admin.sock" {
		t.Errorf("admin listen = %v", admin["listen"])
	}
}

func TestSyncLoadsRoutes(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admin.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	loaded := make(chan string, 1)
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/load" {
				t.Errorf("path = %s", r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			loaded <- string(body)
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	router := NewRouter(Config{
		AdminSocket:       socket,
		Domain:            "ship.test",
		DashboardUpstream: "shipit:8080",
		StaticHost:        "site.test",
		StaticRoot:        "/srv/site",
	}, routes{
		{Project: "live", Container: "shipit-live-3"},
		{Project: "new"},
	})

	if err := router.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := <-loaded
	if !json.Valid([]byte(got)) {
		t.Fatalf("invalid JSON: %s", got)
	}
	for _, want := range []string{
		`"ship.test"`, `"shipit:8080"`,
		`"live.ship.test"`, `"shipit-live-3:3000"`,
		`"new.ship.test"`, `"static_response"`,
		`"site.test"`, `"file_server"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config is missing %s: %s", want, got)
		}
	}
}
