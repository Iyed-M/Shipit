//go:build e2e

package deploy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Iyed-M/Shipit/internal/caddy"
	"github.com/Iyed-M/Shipit/internal/docker"
	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	dockerfile = "FROM busybox:1.37\nCOPY app.sh /app.sh\nCMD [\"sh\", \"/app.sh\"]\n"

	// serve starts a web server showing the version and $COLOUR.
	serve = `mkdir -p /www
echo "$VERSION $COLOUR" > /www/index.html
`
	healthy = serve + `echo ok > /www/healthz
exec httpd -f -p "$PORT" -h /www
`
	// noHealthz answers, but not on the health check path.
	noHealthz = serve + `exec httpd -f -p "$PORT" -h /www
`
	// crashLoop passes its health check and then exits, over and over.
	crashLoop = serve + `echo ok > /www/healthz
httpd -p "$PORT" -h /www
sleep 2
exit 1
`
)

// e2eRepo hands out a different app for each commit.
type e2eRepo struct {
	scripts map[string]string
}

func (r *e2eRepo) Fetch(_ context.Context, _ github.Repo, sha, dir string, _ io.Writer) error {
	files := map[string]string{
		"Dockerfile":  dockerfile,
		"app.sh":      "VERSION=" + sha + "\n" + r.scripts[sha],
		"shipit.toml": "[health]\npath = \"/healthz\"\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (*e2eRepo) SetStatus(context.Context, github.Repo, string, string, string, string) error {
	return nil
}

type e2eRouter struct{}

func (e2eRouter) Sync(context.Context) error { return nil }

type e2e struct {
	t        *testing.T
	store    *store.Store
	pipeline *Pipeline
	repo     *e2eRepo
	project  store.Project
	docker   docker.CLI
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newE2E(t *testing.T) *e2e {
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

	name := "e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	project, err := s.CreateProject(t.Context(), store.Project{
		Name: name, RepoOwner: "o", RepoName: "r", Branch: "main", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}

	dockerOutput(t, "network", "create", name)
	t.Cleanup(func() {
		for _, list := range [][]string{
			{"ps", "--all", "--quiet", "--filter", "label=shipit.project=" + name},
			{"images", "--quiet", "shipit/" + name},
		} {
			ids := strings.Fields(dockerOutput(t, list...))
			if len(ids) == 0 {
				continue
			}
			remove := []string{"rm", "--force"}
			if list[0] == "images" {
				remove = []string{"image", "rm", "--force"}
			}
			dockerOutput(t, append(remove, ids...)...)
		}
		dockerOutput(t, "network", "rm", name)
	})

	repo := &e2eRepo{scripts: map[string]string{}}
	pipeline := NewPipeline(Config{
		DataDir:     dir,
		AppsNetwork: name,
		Domain:      "ship.test",
		Timing: Timing{
			HealthInterval: 100 * time.Millisecond,
			HealthTimeout:  3 * time.Second,
			WatchInterval:  200 * time.Millisecond,
		},
	}, s, docker.CLI{}, NetworkProber{}, repo, e2eRouter{}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeline.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &e2e{t: t, store: s, pipeline: pipeline, repo: repo, project: project}
}

// push deploys the script as a commit and waits for the deploy to end.
func (e *e2e) push(version, script string) store.Deploy {
	e.t.Helper()
	e.repo.scripts[version] = script
	d, _, err := e.store.EnqueueDeploy(e.t.Context(), store.NewDeploy{
		ProjectID: e.project.ID, Kind: store.KindPush, CommitSHA: version, Key: version,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.pipeline.Wake()
	return e.waitFinished(d.ID)
}

func (e *e2e) waitFinished(id int64) store.Deploy {
	e.t.Helper()
	var d store.Deploy
	e.eventually(func() bool {
		var err error
		if d, err = e.store.Deploy(e.t.Context(), id); err != nil {
			e.t.Fatal(err)
		}
		return d.FinishedAt != nil
	}, fmt.Sprintf("deploy %d to finish", id))
	return d
}

func (e *e2e) eventually(ok func() bool, what string) {
	e.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *e2e) liveDeploy() store.Deploy {
	e.t.Helper()
	p, err := e.store.Project(e.t.Context(), e.project.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if p.LiveDeployID == nil {
		e.t.Fatal("nothing is live")
	}
	d, err := e.store.Deploy(e.t.Context(), *p.LiveDeployID)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// page fetches what the live container serves, the way Caddy would reach it.
func (e *e2e) page() string {
	e.t.Helper()
	state, err := e.docker.Inspect(e.t.Context(), e.liveDeploy().Container)
	if err != nil {
		e.t.Fatal(err)
	}
	url := "http://" + net.JoinHostPort(state.IP, strconv.Itoa(caddy.AppPort)) + "/"
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

func (e *e2e) setEnv(name, value string) store.Deploy {
	e.t.Helper()
	if _, err := e.store.UpdateEnv(e.t.Context(), e.project.ID, map[string]*string{name: &value}); err != nil {
		e.t.Fatal(err)
	}
	d, _, err := e.store.EnqueueDeploy(e.t.Context(), store.NewDeploy{
		ProjectID: e.project.ID, Kind: store.KindEnv, CommitSHA: e.liveDeploy().CommitSHA,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.pipeline.Wake()
	return e.waitFinished(d.ID)
}

func (e *e2e) running(container string) bool {
	e.t.Helper()
	out := dockerOutput(e.t, "ps", "--quiet", "--filter", "name=^"+container+"$")
	return out != ""
}

func TestEndToEnd(t *testing.T) {
	e := newE2E(t)

	// A healthy app goes live.
	v1 := e.push("v1", healthy)
	if v1.Status != store.StatusLive {
		t.Fatalf("v1 = %s: %s", v1.Status, v1.Error)
	}

	// An env change is a new release of the same image.
	blue := e.setEnv("COLOUR", "blue")
	if blue.Status != store.StatusLive || blue.Image != v1.Image {
		t.Fatalf("env change = %+v", blue)
	}
	if got := e.page(); got != "v1 blue" {
		t.Fatalf("page = %q", got)
	}
	if e.running(v1.Container) {
		t.Error("the replaced container is still running")
	}

	// A version that fails its health check changes nothing.
	broken := e.push("v2", noHealthz)
	if broken.Status != store.StatusFailed || !strings.Contains(broken.Error, "health check timed out") {
		t.Fatalf("broken = %s: %s", broken.Status, broken.Error)
	}
	if live := e.liveDeploy(); live.ID != blue.ID {
		t.Fatalf("live = %d, want %d", live.ID, blue.ID)
	}
	if got := e.page(); got != "v1 blue" {
		t.Fatalf("page after a failed deploy = %q", got)
	}
	if e.running(broken.Container) {
		t.Error("the failed container is still running")
	}

	// A version that crashes while live restarts, and is rolled back after
	// three crashes.
	crashing := e.push("v3", crashLoop)
	if crashing.Status != store.StatusLive {
		t.Fatalf("crashing = %s: %s", crashing.Status, crashing.Error)
	}
	e.eventually(func() bool {
		d, err := e.store.Deploy(t.Context(), crashing.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d.Status == store.StatusRolledBack
	}, "the crash-looping version to roll back")

	live := e.liveDeploy()
	if live.Kind != store.KindRollback || *live.ReleaseID != *blue.ReleaseID {
		t.Fatalf("live = %+v, want a rollback to release %d", live, *blue.ReleaseID)
	}
	if got := e.page(); got != "v1 blue" {
		t.Errorf("page after the rollback = %q", got)
	}
	crashes, err := e.store.Crashes(t.Context(), e.project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(crashes) < crashLimit {
		t.Errorf("crashes = %d, want at least %d", len(crashes), crashLimit)
	}
}
