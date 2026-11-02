package deploy

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

func TestHealthCheckNeedsConsecutivePasses(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("boom")
	h.prober.results = []error{nil, nil, boom, nil, nil, nil}

	d := h.push(t, "a")

	if d.Status != store.StatusLive {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}
	if len(h.prober.addrs) != 6 {
		t.Errorf("probes = %d, want 6: the failure must restart the count", len(h.prober.addrs))
	}
}

func TestHealthCheckUsesConfiguredPath(t *testing.T) {
	h := newHarness(t)
	h.source.files["shipit.toml"] = "[health]\npath = \"/healthz\"\n"

	h.push(t, "a")

	if h.prober.paths[0] != "/healthz" || h.prober.hosts[0] != "demo.ship.test" || h.prober.addrs[0] != "10.0.0.2:3000" {
		t.Errorf("probe = %s %s %s", h.prober.addrs[0], h.prober.hosts[0], h.prober.paths[0])
	}
}

func TestHealthCheckFallsBackToTCP(t *testing.T) {
	h := newHarness(t)

	h.push(t, "a")

	if h.prober.paths[0] != "" {
		t.Errorf("path = %q, want none", h.prober.paths[0])
	}
}

func TestOldVersionServesUntilHealthCheckPasses(t *testing.T) {
	h := newHarness(t)
	good := h.push(t, "good")
	h.prober.err = errors.New("not ready")

	bad := h.push(t, "bad")

	if h.router.syncs != 1 {
		t.Errorf("syncs = %d, want only the first deploy's", h.router.syncs)
	}
	if slices.Contains(h.runtime.removed, good.Container) {
		t.Error("the serving container was removed")
	}
	logs, err := os.ReadFile(h.pipeline.LogPath(bad.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), "app says goodbye") {
		t.Errorf("deploy log lacks the app's output:\n%s", logs)
	}
}

func TestPushUsesProjectEnv(t *testing.T) {
	h := newHarness(t)
	value := "postgres://db"
	if _, err := h.store.UpdateEnv(t.Context(), h.project.ID, map[string]*string{"DATABASE_URL": &value}); err != nil {
		t.Fatal(err)
	}

	h.push(t, "a")

	env := h.runtime.last().Env
	if env["DATABASE_URL"] != value || env["PORT"] != "3000" {
		t.Errorf("env = %v", env)
	}
}

func TestEnvChangeReleasesSameImageWithoutRebuilding(t *testing.T) {
	h := newHarness(t)
	h.source.files["shipit.toml"] = "[health]\npath = \"/up\"\n\n[resources]\nmemory = \"1g\"\n"
	first := h.push(t, "a")
	h.source.files = nil // Touching the repo would now fail the deploy.

	d := h.changeEnv(t, "FEATURE", "on")

	if d.Status != store.StatusLive {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}
	if h.runtime.builds != 1 {
		t.Errorf("builds = %d, want 1", h.runtime.builds)
	}
	want := []store.Status{store.StatusQueued, store.StatusStarting, store.StatusHealthChecking, store.StatusLive}
	if got := h.stages(t, d.ID); !slices.Equal(got, want) {
		t.Errorf("stages = %v, want %v", got, want)
	}
	c := h.runtime.last()
	if c.Image != first.Image || c.Env["FEATURE"] != "on" || c.Memory != "1g" {
		t.Errorf("container = %+v", c)
	}
	if got := h.prober.paths[len(h.prober.paths)-1]; got != "/up" {
		t.Errorf("health path = %q, want the release's", got)
	}
	if d.ReleaseID == nil || first.ReleaseID == nil || *d.ReleaseID == *first.ReleaseID {
		t.Errorf("release ids = %v, %v, want a new one", first.ReleaseID, d.ReleaseID)
	}
	if !slices.Equal(h.runtime.removed, []string{first.Container}) {
		t.Errorf("removed = %v", h.runtime.removed)
	}
}

func TestBadEnvChangeKeepsCurrentVersion(t *testing.T) {
	h := newHarness(t)
	good := h.push(t, "a")
	h.prober.err = errors.New("bad config")

	d := h.changeEnv(t, "FEATURE", "broken")

	if d.Status != store.StatusFailed {
		t.Fatalf("status = %s", d.Status)
	}
	if live := h.liveDeploy(t); live == nil || *live != good.ID {
		t.Errorf("live deploy = %v, want %d", live, good.ID)
	}
}

func TestEnvChangeDoesNotPostCommitStatus(t *testing.T) {
	h := newHarness(t)
	h.push(t, "a")
	h.source.statuses = nil

	h.changeEnv(t, "FEATURE", "on")

	if len(h.source.statuses) != 0 {
		t.Errorf("statuses = %v", h.source.statuses)
	}
}

func TestOnlyNewestQueuedCommitIsBuilt(t *testing.T) {
	h := newHarness(t)
	older := h.enqueue(t, "a")
	middle := h.enqueue(t, "b")
	newest := h.enqueue(t, "c")

	h.run(t)

	for _, d := range []store.Deploy{older, middle} {
		if got := h.deploy(t, d.ID); got.Status != store.StatusSuperseded {
			t.Errorf("deploy %d = %s, want superseded", d.ID, got.Status)
		}
	}
	if got := h.deploy(t, newest.ID).Status; got != store.StatusLive {
		t.Errorf("newest = %s", got)
	}
	if h.runtime.builds != 1 {
		t.Errorf("builds = %d, want 1", h.runtime.builds)
	}
	if h.source.statuses[0] != github.StateSuccess || h.source.details[0] != "Superseded by a newer commit" {
		t.Errorf("first status = %s %q", h.source.statuses[0], h.source.details[0])
	}
}

func TestSupersedingStaysWithinOneProject(t *testing.T) {
	h := newHarness(t)
	other, err := h.store.CreateProject(t.Context(), store.Project{
		Name: "other", RepoOwner: "o", RepoName: "x", Branch: "main", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := h.enqueue(t, "a")
	if _, _, err := h.store.EnqueueDeploy(t.Context(), store.NewDeploy{
		ProjectID: other.ID, Kind: store.KindPush, CommitSHA: sha, Key: "b",
	}); err != nil {
		t.Fatal(err)
	}

	h.run(t)

	if got := h.deploy(t, first.ID).Status; got != store.StatusLive {
		t.Errorf("deploy = %s, want live", got)
	}
}

func TestKeepsLastThreeImages(t *testing.T) {
	h := newHarness(t)
	var images []string
	for _, delivery := range []string{"a", "b", "c", "d", "e"} {
		images = append(images, h.push(t, delivery).Image)
	}

	if !slices.Equal(h.runtime.removedImages, images[:2]) {
		t.Errorf("removed images = %v, want %v", h.runtime.removedImages, images[:2])
	}
	releases, err := h.store.Releases(t.Context(), h.project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 3 {
		t.Errorf("releases = %d, want 3", len(releases))
	}
}

func TestEnvReleasesShareAnImageForRetention(t *testing.T) {
	h := newHarness(t)
	first := h.push(t, "a")
	for _, value := range []string{"1", "2", "3", "4"} {
		h.changeEnv(t, "N", value)
	}

	if slices.Contains(h.runtime.removedImages, first.Image) {
		t.Error("removed the image still serving")
	}
}

func TestFailedDeploysDontPushOutGoodImages(t *testing.T) {
	h := newHarness(t)
	good := []string{h.push(t, "a").Image, h.push(t, "b").Image, h.push(t, "c").Image}
	h.prober.err = errors.New("unhealthy")
	bad := h.push(t, "d")
	h.push(t, "e")

	for _, image := range good {
		if slices.Contains(h.runtime.removedImages, image) {
			t.Errorf("removed good image %s", image)
		}
	}
	if !slices.Contains(h.runtime.removedImages, bad.Image) {
		t.Errorf("kept failed image %s", bad.Image)
	}
}

func TestFailedTestsRemoveTheImage(t *testing.T) {
	h := newHarness(t)
	h.source.files["shipit.toml"] = "[test]\ncommand = \"false\"\n"
	h.runtime.testErr = errors.New("exit 1")

	d := h.push(t, "a")

	if len(h.runtime.removedImages) != 1 || !strings.HasPrefix(h.runtime.removedImages[0], "shipit/demo:") {
		t.Errorf("removed images = %v (deploy %s)", h.runtime.removedImages, d.Status)
	}
}

func TestRecoverKeepsDeployThatAlreadyTookTraffic(t *testing.T) {
	h := newHarness(t)
	old := h.push(t, "old")

	// Shipit dies after moving traffic but before finishing the deploy.
	d := h.enqueue(t, "new")
	name := containerName(h.project, d)
	release, err := h.store.CreateRelease(t.Context(), store.Release{
		ProjectID: h.project.ID, CommitSHA: sha, Image: "img", Memory: "512m", CPUs: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AssignRelease(t.Context(), d.ID, release, name, &old.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetStage(t.Context(), d.ID, store.StatusHealthChecking); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetLiveDeploy(t.Context(), h.project.ID, &d.ID); err != nil {
		t.Fatal(err)
	}

	if err := h.pipeline.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := h.deploy(t, d.ID).Status; got != store.StatusLive {
		t.Errorf("status = %s, want live", got)
	}
	if slices.Contains(h.runtime.removed, name) {
		t.Error("removed the container taking traffic")
	}
	if !slices.Contains(h.runtime.removed, old.Container) {
		t.Error("left the old container running")
	}
}
