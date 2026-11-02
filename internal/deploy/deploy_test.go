package deploy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Iyed-M/Shipit/internal/docker"
	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

type fakeRuntime struct {
	buildErr, testErr, startErr error
	tests                       []string
	started                     []docker.Container
	removed                     []string
}

func (f *fakeRuntime) Build(context.Context, string, string, io.Writer) error { return f.buildErr }

func (f *fakeRuntime) Test(_ context.Context, _, _, command string, _ io.Writer) error {
	f.tests = append(f.tests, command)
	return f.testErr
}

func (f *fakeRuntime) Start(_ context.Context, c docker.Container) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, c)
	return nil
}

func (f *fakeRuntime) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

type fakeSource struct {
	files    map[string]string
	statuses []string
}

func (f *fakeSource) Fetch(_ context.Context, _ github.Repo, _, dir string, _ io.Writer) error {
	for name, content := range f.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSource) SetStatus(_ context.Context, _ github.Repo, _, state, _, _ string) error {
	f.statuses = append(f.statuses, state)
	return nil
}

type fakeRouter struct {
	err   error
	syncs int
}

func (f *fakeRouter) Sync(context.Context) error {
	f.syncs++
	return f.err
}

type harness struct {
	store    *store.Store
	runtime  *fakeRuntime
	source   *fakeSource
	router   *fakeRouter
	pipeline *Pipeline
	project  store.Project
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "builds"), 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "shipit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	project, err := s.CreateProject(t.Context(), store.Project{
		Name: "demo", RepoOwner: "o", RepoName: "r", Branch: "main", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{
		store:   s,
		runtime: &fakeRuntime{},
		source:  &fakeSource{files: map[string]string{"Dockerfile": "FROM scratch\n"}},
		router:  &fakeRouter{},
		project: project,
	}
	h.pipeline = NewPipeline(Config{DataDir: dir, AppsNetwork: "apps", Domain: "ship.test"},
		s, h.runtime, h.source, h.router, slog.New(slog.DiscardHandler))
	return h
}

// push queues a commit and runs the pipeline until the queue is empty.
func (h *harness) push(t *testing.T, delivery string) store.Deploy {
	t.Helper()
	queued, created, err := h.store.EnqueueDeploy(t.Context(), h.project.ID, sha, "msg", delivery)
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	h.pipeline.drain(t.Context())
	d, err := h.store.Deploy(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (h *harness) liveDeploy(t *testing.T) *int64 {
	t.Helper()
	p, err := h.store.Project(t.Context(), h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p.LiveDeployID
}

func (h *harness) stages(t *testing.T, id int64) []store.Status {
	t.Helper()
	stages, err := h.store.Stages(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make([]store.Status, 0, len(stages))
	for _, s := range stages {
		statuses = append(statuses, s.Status)
	}
	return statuses
}

func TestDeployGoesLive(t *testing.T) {
	h := newHarness(t)

	d := h.push(t, "a")

	if d.Status != store.StatusLive {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}
	want := []store.Status{store.StatusQueued, store.StatusCloning, store.StatusBuilding, store.StatusStarting, store.StatusLive}
	if got := h.stages(t, d.ID); !slices.Equal(got, want) {
		t.Errorf("stages = %v, want %v", got, want)
	}
	if live := h.liveDeploy(t); live == nil || *live != d.ID {
		t.Errorf("live deploy = %v, want %d", live, d.ID)
	}
	if h.router.syncs != 1 {
		t.Errorf("syncs = %d, want 1", h.router.syncs)
	}
	if got := []string{github.StatePending, github.StateSuccess}; !slices.Equal(h.source.statuses, got) {
		t.Errorf("statuses = %v, want %v", h.source.statuses, got)
	}
	c := h.runtime.started[0]
	if c.Env["PORT"] != "3000" || c.Network != "apps" || c.Memory != "512m" || c.CPUs != 0.5 {
		t.Errorf("container = %+v", c)
	}
}

func TestNewDeployReplacesOldContainer(t *testing.T) {
	h := newHarness(t)

	first := h.push(t, "a")
	second := h.push(t, "b")

	if live := h.liveDeploy(t); live == nil || *live != second.ID {
		t.Fatalf("live deploy = %v, want %d", live, second.ID)
	}
	if !slices.Equal(h.runtime.removed, []string{first.Container}) {
		t.Errorf("removed = %v, want only %s", h.runtime.removed, first.Container)
	}
}

func TestTestCommandFromShipitToml(t *testing.T) {
	h := newHarness(t)
	h.source.files["shipit.toml"] = "[test]\ncommand = \"npm test\"\n\n[resources]\nmemory = \"1g\"\n"

	d := h.push(t, "a")

	if d.Status != store.StatusLive {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}
	if !slices.Equal(h.runtime.tests, []string{"npm test"}) {
		t.Errorf("tests = %v", h.runtime.tests)
	}
	if !slices.Contains(h.stages(t, d.ID), store.StatusTesting) {
		t.Error("testing stage not recorded")
	}
	if c := h.runtime.started[0]; c.Memory != "1g" || c.CPUs != 0.5 {
		t.Errorf("limits = %s/%v, want 1g/0.5", c.Memory, c.CPUs)
	}
}

// Every failure path must leave the previous version serving.
func TestFailedDeployKeepsCurrentVersion(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		arrange func(h *harness)
		reason  string
		removes bool
	}{
		{
			name:    "no Dockerfile",
			arrange: func(h *harness) { delete(h.source.files, "Dockerfile") },
			reason:  "no Dockerfile",
		},
		{
			name:    "invalid shipit.toml",
			arrange: func(h *harness) { h.source.files["shipit.toml"] = "[test\n" },
			reason:  "parse shipit.toml",
		},
		{
			name:    "build fails",
			arrange: func(h *harness) { h.runtime.buildErr = boom },
			reason:  "boom",
		},
		{
			name: "tests fail",
			arrange: func(h *harness) {
				h.source.files["shipit.toml"] = "[test]\ncommand = \"false\"\n"
				h.runtime.testErr = boom
			},
			reason: "tests failed",
		},
		{
			name:    "container won't start",
			arrange: func(h *harness) { h.runtime.startErr = boom },
			reason:  "boom",
			removes: true,
		},
		{
			name:    "caddy refuses the route",
			arrange: func(h *harness) { h.router.err = boom },
			reason:  "boom",
			removes: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			good := h.push(t, "good")
			h.source.statuses = nil
			tt.arrange(h)

			bad := h.push(t, "bad")

			if bad.Status != store.StatusFailed || !strings.Contains(bad.Error, tt.reason) {
				t.Errorf("deploy = %s (%q), want failed with %q", bad.Status, bad.Error, tt.reason)
			}
			if live := h.liveDeploy(t); live == nil || *live != good.ID {
				t.Errorf("live deploy = %v, want %d", live, good.ID)
			}
			if slices.Contains(h.runtime.removed, good.Container) {
				t.Error("the serving container was removed")
			}
			if removed := slices.Contains(h.runtime.removed, containerName(h.project, bad)); removed != tt.removes {
				t.Errorf("new container removed = %v, want %v", removed, tt.removes)
			}
			if !slices.Equal(h.source.statuses, []string{github.StatePending, github.StateFailure}) {
				t.Errorf("statuses = %v", h.source.statuses)
			}
		})
	}
}

func TestRecoverFailsInterruptedDeploys(t *testing.T) {
	h := newHarness(t)
	good := h.push(t, "good")

	stuck, _, err := h.store.EnqueueDeploy(t.Context(), h.project.ID, sha, "msg", "stuck")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetStage(t.Context(), stuck.ID, store.StatusBuilding); err != nil {
		t.Fatal(err)
	}
	h.source.statuses = nil

	if err := h.pipeline.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}

	d, err := h.store.Deploy(t.Context(), stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.StatusFailed || d.Error != interruptedReason {
		t.Errorf("deploy = %s (%q)", d.Status, d.Error)
	}
	if live := h.liveDeploy(t); live == nil || *live != good.ID {
		t.Errorf("live deploy = %v, want %d", live, good.ID)
	}
	if !slices.Equal(h.source.statuses, []string{github.StateFailure}) {
		t.Errorf("statuses = %v", h.source.statuses)
	}
}

func TestCancelledDeployIsRecorded(t *testing.T) {
	h := newHarness(t)
	queued, _, err := h.store.EnqueueDeploy(t.Context(), h.project.ID, sha, "msg", "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	h.pipeline.deploy(ctx, queued)

	d, err := h.store.Deploy(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.StatusFailed {
		t.Errorf("status = %s, want failed", d.Status)
	}
}
