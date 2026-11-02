// Package config loads Shipit's settings from environment variables.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// masterKeySize is the AES-256 key length in bytes.
const masterKeySize = 32

// Config holds every setting Shipit reads at startup.
type Config struct {
	// Domain is the dashboard host. Projects live at <project>.<Domain>.
	Domain      string
	AdminToken  string
	GitHubToken string
	// MasterKey seals env vars in the database.
	MasterKey []byte

	Listen  string
	DataDir string

	CaddyAdminSocket string
	// CaddyUpstream is the address Caddy uses to reach this process.
	CaddyUpstream string
	// StaticHost, when set, is served from StaticRoot inside the Caddy container.
	StaticHost string
	StaticRoot string

	AppsNetwork string

	GitHubAPIURL string
	GitHubWebURL string
}

// Load reads the environment and reports every missing required variable at once.
func Load() (Config, error) {
	c := Config{
		Domain:           os.Getenv("SHIPIT_DOMAIN"),
		AdminToken:       os.Getenv("SHIPIT_ADMIN_TOKEN"),
		GitHubToken:      os.Getenv("SHIPIT_GITHUB_TOKEN"),
		Listen:           env("SHIPIT_LISTEN", ":8080"),
		DataDir:          env("SHIPIT_DATA_DIR", "/data"),
		CaddyAdminSocket: env("SHIPIT_CADDY_ADMIN_SOCKET", "/run/caddy/admin.sock"),
		CaddyUpstream:    env("SHIPIT_CADDY_UPSTREAM", "shipit:8080"),
		StaticHost:       os.Getenv("SHIPIT_STATIC_HOST"),
		StaticRoot:       env("SHIPIT_STATIC_ROOT", "/srv/site"),
		AppsNetwork:      env("SHIPIT_APPS_NETWORK", "shipit-apps"),
		GitHubAPIURL:     strings.TrimRight(env("SHIPIT_GITHUB_API_URL", "https://api.github.com"), "/"),
		GitHubWebURL:     strings.TrimRight(env("SHIPIT_GITHUB_WEB_URL", "https://github.com"), "/"),
	}

	var missing []string
	for name, value := range map[string]string{
		"SHIPIT_DOMAIN":       c.Domain,
		"SHIPIT_ADMIN_TOKEN":  c.AdminToken,
		"SHIPIT_GITHUB_TOKEN": c.GitHubToken,
		"SHIPIT_MASTER_KEY":   os.Getenv("SHIPIT_MASTER_KEY"),
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	key, err := hex.DecodeString(os.Getenv("SHIPIT_MASTER_KEY"))
	if err != nil || len(key) != masterKeySize {
		return Config{}, fmt.Errorf("SHIPIT_MASTER_KEY must be %d hex characters: use openssl rand -hex %d",
			masterKeySize*2, masterKeySize)
	}
	c.MasterKey = key
	return c, nil
}

// PublicURL is the HTTPS origin of the dashboard and webhook endpoint.
func (c Config) PublicURL() string {
	return "https://" + c.Domain
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
