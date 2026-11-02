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
	"sync"
	"testing"
	"time"

	"github.com/Iyed-M/Shipit/internal/docker"
	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

type fakeRuntime struct {
	mu                          sync.Mutex
	buildErr, testErr, startErr error
	// onBuild, if set, runs during a build.
	onBuild       func()
	builds        int
	tests         []string
	started       []docker.Container
	removed       []string
	removedImages []string
	// states overrides what Inspect says about a container, which is otherwise
	// a healthy one.
	states map[string]docker.State
}

func (f *fakeRuntime) Build(context.Context, string, string, io.Writer) error {
	f.mu.Lock()
	f.builds++
	onBuild := f.onBuild
	f.mu.Unlock()
	if onBuild != nil {
		onBuild()
	}
	return f.buildErr
}

func (f *fakeRuntime) Test(_ context.Context, _, _, command string, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tests = append(f.tests, command)
	return f.testErr
}

func (f *fakeRuntime) Start(_ context.Context, c docker.Container) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, c)
	return nil
}

func (f *fakeRuntime) Inspect(_ context.Context, name string) (docker.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state, ok := f.states[name]; ok {
		return state, nil
	}
	return docker.State{Running: true, IP: "10.0.0.2"}, nil
}

func (f *fakeRuntime) setState(name string, state docker.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[string]docker.State{}
	}
	f.states[name] = state
}

func (f *fakeRuntime) Logs(_ context.Context, _ string, _ int, w io.Writer) error {
	_, err := io.WriteString(w, "app says goodbye\n")
	return err
}

func (f *fakeRuntime) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeRuntime) RemoveImage(_ context.Context, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedImages = append(f.removedImages, tag)
	return nil
}

// last returns the most recently started container.
func (f *fakeRuntime) last() docker.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[len(f.started)-1]
}

// fakeProber passes every check unless results scripts the next ones.
type fakeProber struct {
	mu      sync.Mutex
	err     error
	results []error
	addrs   []string
	hosts   []string
	paths   []string
}

func (f *fakeProber) Probe(_ context.Context, addr, host, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs = append(f.addrs, addr)
	f.hosts = append(f.hosts, host)
	f.paths = append(f.paths, path)
	if len(f.results) > 0 {
		err := f.results[0]
		f.results = f.results[1:]
		return err
	}
	return f.err
}

type fakeSource struct {
	mu       sync.Mutex
	files    map[string]string
	statuses []string
	details  []string
}

func (f *fakeSource) Fetch(_ context.Context, _ github.Repo, _, dir string, _ io.Writer) error {
	for name, content := range f.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSource) SetStatus(_ context.Context, _ github.Repo, _, state, description, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, state)
	f.details = append(f.details, description)
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
	prober   *fakeProber
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
	s, err := store.Open(filepath.Join(dir, "shipit.db"), make([]byte, 32))
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
		prober:  &fakeProber{},
		source:  &fakeSource{files: map[string]string{"Dockerfile": "FROM scratch\n"}},
		router:  &fakeRouter{},
		project: project,
	}
	h.pipeline = NewPipeline(Config{
		DataDir:     dir,
		AppsNetwork: "apps",
		Domain:      "ship.test",
		Timing:      Timing{HealthTimeout: 50 * time.Millisecond, HealthInterval: time.Millisecond},
	}, s, h.runtime, h.prober, h.source, h.router, slog.New(slog.DiscardHandler))
	return h
}

// enqueue queues a push without running it.
func (h *harness) enqueue(t *testing.T, delivery string) store.Deploy {
	t.Helper()
	d, created, err := h.store.EnqueueDeploy(t.Context(), store.NewDeploy{
		ProjectID: h.project.ID, Kind: store.KindPush, CommitSHA: sha, Message: "msg", Key: delivery,
	})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	return d
}

// run works through both queue lanes.
func (h *harness) run(t *testing.T) {
	t.Helper()
	h.pipeline.drain(t.Context(), store.LaneBuild)
	h.pipeline.drain(t.Context(), store.LaneRelease)
}

func (h *harness) deploy(t *testing.T, id int64) store.Deploy {
	t.Helper()
	d, err := h.store.Deploy(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// changeEnv changes a var the way the API does, and applies it.
func (h *harness) changeEnv(t *testing.T, name, value string) store.Deploy {
	t.Helper()
	if _, err := h.store.UpdateEnv(t.Context(), h.project.ID, map[string]*string{name: &value}); err != nil {
		t.Fatal(err)
	}
	queued, _, err := h.store.EnqueueDeploy(t.Context(), store.NewDeploy{
		ProjectID: h.project.ID, Kind: store.KindEnv, CommitSHA: sha, Message: "Update env vars",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	return h.deploy(t, queued.ID)
}

// push queues a commit and runs the pipeline until the queue is empty.
func (h *harness) push(t *testing.T, delivery string) store.Deploy {
	t.Helper()
	queued := h.enqueue(t, delivery)
	h.run(t)
	return h.deploy(t, queued.ID)
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
	want := []store.Status{
		store.StatusQueued, store.StatusCloning, store.StatusBuilding,
		store.StatusStarting, store.StatusHealthChecking, store.StatusLive,
	}
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
		{
			name:    "health check never passes",
			arrange: func(h *harness) { h.prober.err = boom },
			reason:  "health check timed out",
			removes: true,
		},
		{
			name: "app exits during the health check",
			arrange: func(h *harness) {
				h.runtime.setState("shipit-demo-2", docker.State{Running: true, Restarting: true, ExitCode: 7})
			},
			reason:  "exited with code 7",
			removes: true,
		},
		{
			name: "app restarts during the health check",
			arrange: func(h *harness) {
				h.runtime.setState("shipit-demo-2", docker.State{Running: true, IP: "10.0.0.2", RestartCount: 1})
			},
			reason:  "crashed during the health check",
			removes: true,
		},
		{
			name: "app runs out of memory",
			arrange: func(h *harness) {
				h.runtime.setState("shipit-demo-2", docker.State{OOMKilled: true})
			},
			reason:  "out of memory",
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

	stuck := h.enqueue(t, "stuck")
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
	queued := h.enqueue(t, "a")
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
