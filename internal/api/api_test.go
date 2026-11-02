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
	"time"

	"github.com/Iyed-M/Shipit/internal/deploy"
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

type fakeQueue struct {
	wakes       int
	rollbacks   []*int64
	rollbackErr error
}

func (f *fakeQueue) Wake()                { f.wakes++ }
func (f *fakeQueue) LogPath(int64) string { return "/nonexistent" }

func (f *fakeQueue) Rollback(_ context.Context, project store.Project, releaseID *int64) (store.Deploy, error) {
	f.rollbacks = append(f.rollbacks, releaseID)
	if f.rollbackErr != nil {
		return store.Deploy{}, f.rollbackErr
	}
	return store.Deploy{ProjectID: project.ID, Kind: store.KindRollback}, nil
}

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

func (h *harness) request(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// seedLive gives the project a deploy that is serving.
func (h *harness) seedLive(t *testing.T, project store.Project) store.Deploy {
	t.Helper()
	d, _, err := h.store.EnqueueDeploy(t.Context(), store.NewDeploy{
		ProjectID: project.ID, Kind: store.KindPush, CommitSHA: "abc", Key: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.FinishDeploy(t.Context(), d.ID, store.StatusLive, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetLiveDeploy(t.Context(), project.ID, &d.ID); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestEnvNeverReturnsValues(t *testing.T) {
	h := newHarness(t)
	h.seedProject(t)

	rec := h.request(http.MethodPatch, "/api/projects/demo/env", `{"B_KEY":"hunter2","A_KEY":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	for _, rec := range []*httptest.ResponseRecorder{rec, h.request(http.MethodGet, "/api/projects/demo/env", "")} {
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("response leaks a value: %s", rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"keys":["A_KEY","B_KEY"]`) {
			t.Errorf("body = %s", rec.Body)
		}
	}
}

func TestEnvChangeWithNothingLiveOnlySaves(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)

	h.request(http.MethodPatch, "/api/projects/demo/env", `{"KEY":"value"}`)

	deploys, err := h.store.Deploys(t.Context(), project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 0 {
		t.Errorf("deploys = %+v, want none", deploys)
	}
}

func TestEnvChangeQueuesDeploy(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)
	h.seedLive(t, project)

	rec := h.request(http.MethodPatch, "/api/projects/demo/env", `{"KEY":"value"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	deploys, err := h.store.Deploys(t.Context(), project.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deploys[0].Kind != store.KindEnv || deploys[0].Status != store.StatusQueued || deploys[0].CommitSHA != "abc" {
		t.Errorf("deploy = %+v", deploys[0])
	}
	if h.queue.wakes != 1 {
		t.Errorf("wakes = %d", h.queue.wakes)
	}
}

func TestEnvNullRemovesVariable(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)

	h.request(http.MethodPatch, "/api/projects/demo/env", `{"KEEP":"1","DROP":"2"}`)
	h.request(http.MethodPatch, "/api/projects/demo/env", `{"DROP":null}`)

	env, err := h.store.Env(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env["KEEP"] != "1" {
		t.Errorf("env = %v", env)
	}
}

func TestEnvRejections(t *testing.T) {
	h := newHarness(t)
	h.seedProject(t)

	for name, body := range map[string]string{
		"not JSON":    `nope`,
		"no changes":  `{}`,
		"bad name":    `{"NOT VALID":"x"}`,
		"reserved":    `{"PORT":"8000"}`,
		"not a value": `{"KEY":1}`,
		"NUL byte":    `{"KEY":"a\u0000b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := h.request(http.MethodPatch, "/api/projects/demo/env", body); rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestRollback(t *testing.T) {
	h := newHarness(t)
	h.seedProject(t)

	rec := h.request(http.MethodPost, "/api/projects/demo/rollback", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	if h.request(http.MethodPost, "/api/projects/demo/rollback", `{"release_id":7}`).Code != http.StatusAccepted {
		t.Fatal("rollback with a release id refused")
	}
	if h.queue.rollbacks[0] != nil || *h.queue.rollbacks[1] != 7 {
		t.Errorf("rollbacks = %v", h.queue.rollbacks)
	}
}

func TestRollbackWithoutTargetConflicts(t *testing.T) {
	h := newHarness(t)
	h.seedProject(t)
	h.queue.rollbackErr = deploy.ErrNoRollbackTarget

	if rec := h.request(http.MethodPost, "/api/projects/demo/rollback", ""); rec.Code != http.StatusConflict {
		t.Errorf("code = %d: %s", rec.Code, rec.Body)
	}
}

func TestListCrashesAndReleases(t *testing.T) {
	h := newHarness(t)
	project := h.seedProject(t)
	d := h.seedLive(t, project)
	if err := h.store.RecordCrashes(t.Context(), d.ID, 2, 2, time.Now()); err != nil {
		t.Fatal(err)
	}

	var crashes []store.Crash
	rec := h.request(http.MethodGet, "/api/projects/demo/crashes", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &crashes); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if len(crashes) != 2 {
		t.Errorf("crashes = %+v", crashes)
	}
	if rec := h.request(http.MethodGet, "/api/projects/demo/releases", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("releases = %s", rec.Body)
	}
}
