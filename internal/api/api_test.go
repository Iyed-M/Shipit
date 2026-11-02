package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	adminToken = "admin"
	secret     = "hook-secret"
)

type fakeGitHub struct {
	err    error
	target string
}

func (f *fakeGitHub) CreateWebhook(_ context.Context, _ github.Repo, target, _ string) (int64, error) {
	f.target = target
	return 42, f.err
}

type fakeRouter struct{ syncs int }

func (f *fakeRouter) Sync(context.Context) error {
	f.syncs++
	return nil
}

type fakeQueue struct{ wakes int }

func (f *fakeQueue) Wake()                { f.wakes++ }
func (f *fakeQueue) LogPath(int64) string { return "/nonexistent" }

type harness struct {
	store   *store.Store
	github  *fakeGitHub
	router  *fakeRouter
	queue   *fakeQueue
	handler http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "shipit.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	h := &harness{store: s, github: &fakeGitHub{}, router: &fakeRouter{}, queue: &fakeQueue{}}
	cfg := Config{AdminToken: adminToken, PublicURL: "https://ship.test", Domain: "ship.test"}
	h.handler = New(cfg, s, h.github, h.router, h.queue, slog.New(slog.DiscardHandler)).Handler()
	return h
}

func (h *harness) createProject(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) seedProject(t *testing.T) store.Project {
	t.Helper()
	p, err := h.store.CreateProject(t.Context(), store.Project{
		Name: "demo", RepoOwner: "o", RepoName: "r", Branch: "main", WebhookSecret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) webhook(event, delivery, signature, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/demo", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signature)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushBody(branch string) string {
	return fmt.Sprintf(`{"ref":"refs/heads/%s","after":"abc","head_commit":{"message":"first\nsecond"}}`, branch)
}

func TestCreateProject(t *testing.T) {
	h := newHarness(t)

	rec := h.createProject(t, `{"repo_url":"https://github.com/Iyed-M/My_App.git"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Name      string `json:"name"`
		Branch    string `json:"branch"`
		URL       string `json:"url"`
		WebhookID int64  `json:"webhook_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-app" || got.Branch != "main" || got.URL != "https://my-app.ship.test" || got.WebhookID != 42 {
		t.Errorf("project = %+v", got)
	}
	if h.github.target != "https://ship.test/webhooks/github/my-app" {
		t.Errorf("webhook target = %s", h.github.target)
	}
	if h.router.syncs != 1 {
		t.Errorf("syncs = %d, want the route added at creation", h.router.syncs)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("response leaks the webhook secret: %s", rec.Body)
	}
}

func TestCreateProjectRejections(t *testing.T) {
	h := newHarness(t)
	h.createProject(t, `{"repo_url":"https://github.com/o/taken"}`)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"not JSON", `nope`, http.StatusBadRequest},
		{"not GitHub", `{"repo_url":"https://gitlab.com/o/r"}`, http.StatusBadRequest},
		{"bad name", `{"repo_url":"https://github.com/o/r","name":"Not Valid"}`, http.StatusBadRequest},
		{"taken name", `{"repo_url":"https://github.com/o/taken"}`, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := h.createProject(t, tt.body); rec.Code != tt.code {
				t.Errorf("code = %d, want %d: %s", rec.Code, tt.code, rec.Body)
			}
		})
	}
}

func TestProjectIsNotKeptWithoutWebhook(t *testing.T) {
	h := newHarness(t)
	h.github.err = errors.New("no access")

	rec := h.createProject(t, `{"repo_url":"https://github.com/o/r"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, err := h.store.ProjectByName(t.Context(), "r"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("project still present: err = %v", err)
	}
}

func TestAPIRequiresToken(t *testing.T) {
	h := newHarness(t)
	for _, header := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: code = %d", header, rec.Code)
		}
	}
}

func TestWebhookQueuesDeploy(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)
	body := pushBody("main")

	if rec := h.webhook("push", "d1", sign(body), body); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}

	deploys, err := h.store.Deploys(t.Context(), project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 1 || deploys[0].CommitSHA != "abc" || deploys[0].CommitMessage != "first" {
		t.Errorf("deploys = %+v", deploys)
	}
	if h.queue.wakes != 1 {
		t.Errorf("wakes = %d", h.queue.wakes)
	}
}

func TestWebhookIgnoresRepeatedDelivery(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)
	body := pushBody("main")

	h.webhook("push", "d1", sign(body), body)
	if rec := h.webhook("push", "d1", sign(body), body); rec.Code != http.StatusNoContent {
		t.Errorf("code = %d", rec.Code)
	}

	deploys, err := h.store.Deploys(t.Context(), project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 1 {
		t.Errorf("deploys = %d, want 1", len(deploys))
	}
}

func TestWebhookDoesNotDeploy(t *testing.T) {
	deleted := `{"ref":"refs/heads/main","deleted":true,"after":"000"}`
	tag := `{"ref":"refs/tags/main","after":"abc"}`
	tests := []struct {
		name  string
		event string
		sig   string
		body  string
		code  int
	}{
		{"bad signature", "push", "sha256=00", pushBody("main"), http.StatusUnauthorized},
		{"no signature", "push", "", pushBody("main"), http.StatusUnauthorized},
		{"ping", "ping", sign("{}"), "{}", http.StatusNoContent},
		{"other branch", "push", sign(pushBody("dev")), pushBody("dev"), http.StatusNoContent},
		{"branch deleted", "push", sign(deleted), deleted, http.StatusNoContent},
		{"tag named like the branch", "push", sign(tag), tag, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			project := h.seedProject(t)

			if rec := h.webhook(tt.event, "d1", tt.sig, tt.body); rec.Code != tt.code {
				t.Errorf("code = %d, want %d", rec.Code, tt.code)
			}

			deploys, err := h.store.Deploys(t.Context(), project.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(deploys) != 0 {
				t.Errorf("deploys = %+v, want none", deploys)
			}
		})
	}
}

func TestWebhookUnknownProject(t *testing.T) {
	h := newHarness(t)
	if rec := h.webhook("push", "d1", "", "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}
