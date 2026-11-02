package deploy

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Iyed-M/Shipit/internal/docker"
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

func TestRollbackRestoresReleaseWithItsEnv(t *testing.T) {
	h := newHarness(t)
	first := h.push(t, "a")
	h.changeEnv(t, "COLOUR", "red") // first's image, red.
	red, _ := h.store.Deploys(t.Context(), h.project.ID, 1)
	h.changeEnv(t, "COLOUR", "blue")
	bad := h.push(t, "b")
	h.source.statuses = nil
	builds := h.runtime.builds

	d := h.rollback(t, red[0].ReleaseID)

	if d.Status != store.StatusLive {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}
	if h.runtime.builds != builds {
		t.Error("rollback rebuilt the image")
	}
	c := h.runtime.last()
	if c.Image != first.Image || c.Env["COLOUR"] != "red" {
		t.Errorf("container = %+v", c)
	}
	env, err := h.store.Env(t.Context(), h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env["COLOUR"] != "red" {
		t.Errorf("project env = %v, want the release's", env)
	}
	if live := h.liveDeploy(t); live == nil || *live != d.ID {
		t.Errorf("live deploy = %v, want %d", live, d.ID)
	}
	replaced := h.deploy(t, bad.ID)
	if replaced.Status != store.StatusRolledBack || replaced.Error != d.CommitMessage {
		t.Errorf("replaced deploy = %s (%q)", replaced.Status, replaced.Error)
	}
	if !slices.Contains(h.runtime.removed, bad.Container) {
		t.Error("the replaced container is still running")
	}
	if !slices.Equal(h.source.statuses, []string{github.StateFailure}) {
		t.Errorf("statuses = %v, want the replaced commit marked as failed", h.source.statuses)
	}
}

func TestRollbackDefaultsToPreviousRelease(t *testing.T) {
	h := newHarness(t)
	first := h.push(t, "a")
	h.push(t, "b")

	d := h.rollback(t, nil)

	if d.ReleaseID == nil || *d.ReleaseID != *first.ReleaseID {
		t.Errorf("release = %v, want %v", d.ReleaseID, first.ReleaseID)
	}
}

func TestRollbackThatFailsHealthCheckKeepsCurrentVersion(t *testing.T) {
	h := newHarness(t)
	h.push(t, "a")
	second := h.push(t, "b")
	h.prober.err = errors.New("old release no longer starts")

	d := h.rollback(t, nil)

	if d.Status != store.StatusFailed {
		t.Fatalf("status = %s", d.Status)
	}
	if live := h.liveDeploy(t); live == nil || *live != second.ID {
		t.Errorf("live deploy = %v, want %d", live, second.ID)
	}
	if got := h.deploy(t, second.ID).Status; got != store.StatusLive {
		t.Errorf("current deploy = %s, want it untouched", got)
	}
}

func TestRollbackRefusals(t *testing.T) {
	h := newHarness(t)
	if _, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), nil); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("nothing live: err = %v", err)
	}

	first := h.push(t, "a")
	if _, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), nil); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("first deploy: err = %v", err)
	}
	if _, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), first.ReleaseID); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("already live: err = %v", err)
	}

	h.prober.err = errors.New("unhealthy")
	failed := h.push(t, "b")
	h.prober.err = nil
	if _, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), failed.ReleaseID); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("never went live: err = %v", err)
	}
}

func TestStaleRollbackIsSuperseded(t *testing.T) {
	h := newHarness(t)
	h.push(t, "a")
	h.push(t, "b")
	queued, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	fix := h.enqueue(t, "fix")

	h.pipeline.drain(t.Context(), store.LaneBuild) // The fix gets there first.
	h.pipeline.drain(t.Context(), store.LaneRelease)

	if got := h.deploy(t, queued.ID).Status; got != store.StatusSuperseded {
		t.Errorf("rollback = %s, want superseded", got)
	}
	if live := h.liveDeploy(t); live == nil || *live != fix.ID {
		t.Errorf("live deploy = %v, want the fix, %d", live, fix.ID)
	}
}

func TestRollbackDoesNotWaitForABuild(t *testing.T) {
	h := newHarness(t)
	h.push(t, "a")
	h.push(t, "b")

	building := make(chan struct{})
	release := make(chan struct{})
	h.runtime.onBuild = func() {
		close(building)
		<-release
	}
	h.enqueue(t, "slow")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.pipeline.drain(context.Background(), store.LaneBuild)
	}()
	<-building

	queued, err := h.pipeline.Rollback(t.Context(), h.currentProject(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.pipeline.drain(t.Context(), store.LaneRelease)

	if got := h.deploy(t, queued.ID).Status; got != store.StatusLive {
		t.Errorf("rollback = %s, want it live while the build runs", got)
	}
	h.runtime.mu.Lock()
	h.runtime.onBuild = nil
	h.runtime.mu.Unlock()
	close(release)
	<-done
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

func TestCrashesAreRecorded(t *testing.T) {
	h := newHarness(t)
	d := h.push(t, "a")
	at := time.Now()

	h.crash(t, d.Container, 1, at)
	h.crash(t, d.Container, 1, at) // Same count: already recorded.

	crashes, err := h.store.Crashes(t.Context(), h.project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(crashes) != 1 || crashes[0].DeployID != d.ID {
		t.Fatalf("crashes = %+v", crashes)
	}
	if !crashes[0].CrashedAt.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("crashed at %v, want %v", crashes[0].CrashedAt, at)
	}
	if live := h.liveDeploy(t); live == nil || *live != d.ID {
		t.Errorf("one crash changed what is live: %v", live)
	}
}

func TestCrashLoopRollsBack(t *testing.T) {
	h := newHarness(t)
	good := h.push(t, "good")
	bad := h.push(t, "bad")
	h.source.statuses = nil

	at := time.Now()
	h.crash(t, bad.Container, 2, at)
	h.run(t)
	if live := h.liveDeploy(t); *live != bad.ID {
		t.Fatal("rolled back after 2 crashes")
	}
	h.crash(t, bad.Container, 3, at.Add(time.Second))
	h.crash(t, bad.Container, 4, at.Add(2*time.Second)) // Must not queue a second one.
	h.run(t)

	live := h.liveDeploy(t)
	if live == nil || *live == bad.ID {
		t.Fatalf("live deploy = %v, want a rollback", live)
	}
	rollback := h.deploy(t, *live)
	if rollback.Kind != store.KindRollback || *rollback.ReleaseID != *good.ReleaseID {
		t.Errorf("live = %+v, want a rollback to release %d", rollback, *good.ReleaseID)
	}
	replaced := h.deploy(t, bad.ID)
	if replaced.Status != store.StatusRolledBack || !strings.Contains(replaced.Error, "crashed 3 times") {
		t.Errorf("bad deploy = %s (%q)", replaced.Status, replaced.Error)
	}
	if !slices.Equal(h.source.statuses, []string{github.StateFailure}) {
		t.Errorf("statuses = %v", h.source.statuses)
	}
	deploys, err := h.store.Deploys(t.Context(), h.project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 3 {
		t.Errorf("deploys = %d, want exactly one rollback", len(deploys))
	}
}

func TestCrashesAfterTheWindowDontRollBack(t *testing.T) {
	h := newHarness(t)
	h.push(t, "good")
	bad := h.push(t, "bad")

	h.crash(t, bad.Container, 3, bad.FinishedAt.Add(crashWindow+time.Minute))
	h.run(t)

	if live := h.liveDeploy(t); *live != bad.ID {
		t.Errorf("live deploy = %d, want %d", *live, bad.ID)
	}
}

func TestCrashLoopWithNothingToRestore(t *testing.T) {
	h := newHarness(t)
	only := h.push(t, "a")

	h.crash(t, only.Container, 3, time.Now())
	h.run(t)

	if live := h.liveDeploy(t); *live != only.ID {
		t.Errorf("live deploy = %d, want %d", *live, only.ID)
	}
}

func TestCrashingRollbackDoesNotRollBackAgain(t *testing.T) {
	h := newHarness(t)
	h.push(t, "good")
	h.push(t, "bad")
	rollback := h.rollback(t, nil)

	h.crash(t, rollback.Container, 3, time.Now())
	h.run(t)

	if live := h.liveDeploy(t); *live != rollback.ID {
		t.Errorf("live deploy = %d, want the rollback, %d", *live, rollback.ID)
	}
}

func TestCrashesDuringAnOutageAreCounted(t *testing.T) {
	h := newHarness(t)
	h.push(t, "good")
	bad := h.push(t, "bad")

	// A new pipeline over the same database, as after a restart.
	restarted := NewPipeline(h.pipeline.cfg, h.store, h.runtime, h.prober, h.source, h.router, h.pipeline.log)
	h.runtime.setState(bad.Container, docker.State{Running: true, RestartCount: 3, FinishedAt: time.Now()})
	restarted.checkCrashes(t.Context())
	restarted.drain(t.Context(), store.LaneRelease)

	if live := h.liveDeploy(t); *live == bad.ID {
		t.Error("crashes missed while Shipit was down didn't roll back")
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
