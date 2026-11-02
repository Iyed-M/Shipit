package deploy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeHTTP(t *testing.T) {
	var host string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	addr := server.Listener.Addr().String()

	if err := (NetworkProber{}).Probe(t.Context(), addr, "demo.ship.test", "/ok"); err != nil {
		t.Errorf("2xx: %v", err)
	}
	if host != "demo.ship.test" {
		t.Errorf("host = %q", host)
	}
	if err := (NetworkProber{}).Probe(t.Context(), addr, "h", "/broken"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: err = %v", err)
	}
	if err := (NetworkProber{}).Probe(t.Context(), addr, "h", "/redirect"); err == nil {
		t.Error("a redirect passed")
	}
}

func TestProbeTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	if err := (NetworkProber{}).Probe(t.Context(), addr, "h", ""); err != nil {
		t.Errorf("open port: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (NetworkProber{}).Probe(t.Context(), addr, "h", ""); err == nil {
		t.Error("closed port passed")
	}
}
