// Package deploy runs the pipeline that takes a commit from queued to live.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Iyed-M/Shipit/internal/caddy"
	"github.com/Iyed-M/Shipit/internal/docker"
	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/shipfile"
	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	// buildTimeout bounds cloning and building together.
	buildTimeout = 15 * time.Minute
	testTimeout  = 10 * time.Minute
	stepTimeout  = 2 * time.Minute

	// failureLogLines is how much of a failed app's output goes in the deploy log.
	failureLogLines = 200

	interruptedReason = "interrupted by a Shipit restart"
)

// errSuperseded stops a deploy that events have overtaken.
var errSuperseded = errors.New("superseded")

// Runtime runs images. It is the seam that lets tests use a fake Docker.
type Runtime interface {
	Build(ctx context.Context, dir, tag string, log io.Writer) error
	Test(ctx context.Context, name, image, command string, log io.Writer) error
	Start(ctx context.Context, c docker.Container) error
	Inspect(ctx context.Context, name string) (docker.State, error)
	Logs(ctx context.Context, name string, lines int, w io.Writer) error
	Remove(ctx context.Context, name string) error
	RemoveImage(ctx context.Context, tag string) error
}

// Source fetches a repo's code and reports results back to it.
type Source interface {
	Fetch(ctx context.Context, repo github.Repo, sha, dir string, log io.Writer) error
	SetStatus(ctx context.Context, repo github.Repo, sha, state, description, targetURL string) error
}

// Router publishes routes from the store.
type Router interface {
	Sync(ctx context.Context) error
}

// Config holds the pipeline's settings.
type Config struct {
	DataDir     string
	AppsNetwork string
	Domain      string
	Timing      Timing
}

// Timing holds the pipeline's waits.
type Timing struct {
	// HealthInterval is the pause between health checks.
	HealthInterval time.Duration
	// HealthTimeout is how long an app has to pass its health check.
	HealthTimeout time.Duration
	// Drain is how long the old version stays up after traffic leaves it, so
	// requests it was serving can finish.
	Drain time.Duration
}

// DefaultTiming is what Shipit runs with.
func DefaultTiming() Timing {
	return Timing{
		HealthInterval: time.Second,
		HealthTimeout:  time.Minute,
		Drain:          2 * time.Second,
	}
}

// Pipeline takes deploys from the queue to live.
type Pipeline struct {
	cfg     Config
	store   *store.Store
	runtime Runtime
	prober  Prober
	source  Source
	router  Router
	log     *slog.Logger
	// wakes has one channel per lane.
	wakes map[store.Lane]chan struct{}
	// activation lets one deploy at a time change what is serving, and keeps
	// image cleanup from running while it does.
	activation sync.Mutex
}

// NewPipeline returns a Pipeline using the given collaborators.
func NewPipeline(cfg Config, s *store.Store, r Runtime, prober Prober, src Source, router Router, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg:     cfg,
		store:   s,
		runtime: r,
		prober:  prober,
		source:  src,
		router:  router,
		log:     log,
		wakes: map[store.Lane]chan struct{}{
			store.LaneBuild:   make(chan struct{}, 1),
			store.LaneRelease: make(chan struct{}, 1),
		},
	}
}

// Wake tells the workers that a deploy has been queued.
func (p *Pipeline) Wake() {
	for _, wake := range p.wakes {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Run works through the queue until ctx ends.
//
// One build runs at a time, across all projects, so builds never compete for
// the server. Env changes and rollbacks have their own worker: they don't build,
// and a crash-looping app shouldn't wait for another project's build to finish.
func (p *Pipeline) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for lane := range p.wakes {
		workers.Go(func() { p.work(ctx, lane) })
	}
	workers.Wait()
}

func (p *Pipeline) work(ctx context.Context, lane store.Lane) {
	for {
		p.drain(ctx, lane)
		select {
		case <-ctx.Done():
			return
		case <-p.wakes[lane]:
		}
	}
}

func (p *Pipeline) drain(ctx context.Context, lane store.Lane) {
	for ctx.Err() == nil {
		d, err := p.store.NextQueued(ctx, lane)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			p.log.Error("read deploy queue", "err", err)
			return
		}
		p.deploy(ctx, d)
	}
}

// Recover clears up deploys left mid-pipeline by a previous process. If one had
// already taken traffic it stays live and finishes going live. Otherwise the
// version that was serving is untouched, and the deploy has failed.
func (p *Pipeline) Recover(ctx context.Context) error {
	interrupted, err := p.store.InFlightDeploys(ctx)
	if err != nil {
		return fmt.Errorf("find interrupted deploys: %w", err)
	}
	for _, d := range interrupted {
		project, err := p.store.Project(ctx, d.ProjectID)
		if err != nil {
			return fmt.Errorf("load project of deploy %d: %w", d.ID, err)
		}
		if project.LiveDeployID != nil && *project.LiveDeployID == d.ID {
			if err := p.store.FinishDeploy(ctx, d.ID, store.StatusLive, ""); err != nil {
				return fmt.Errorf("finish deploy %d: %w", d.ID, err)
			}
			p.report(ctx, project, d, github.StateSuccess, "Live")
			p.retire(ctx, project, d)
			p.log.Warn("finished interrupted deploy", "project", project.Name, "deploy", d.ID)
			continue
		}
		if err := p.runtime.Remove(ctx, containerName(project, d)); err != nil {
			p.log.Warn("remove interrupted container", "deploy", d.ID, "err", err)
		}
		if err := p.runtime.Remove(ctx, testContainerName(project, d)); err != nil {
			p.log.Warn("remove interrupted test container", "deploy", d.ID, "err", err)
		}
		if err := p.store.FinishDeploy(ctx, d.ID, store.StatusFailed, interruptedReason); err != nil {
			return fmt.Errorf("fail deploy %d: %w", d.ID, err)
		}
		p.report(ctx, project, d, github.StateFailure, interruptedReason)
		p.log.Warn("failed interrupted deploy", "project", project.Name, "deploy", d.ID)
	}
	return nil
}

// LogPath is where a deploy's output is written.
func (p *Pipeline) LogPath(id int64) string {
	return filepath.Join(p.cfg.DataDir, "logs", strconv.FormatInt(id, 10)+".log")
}

func (p *Pipeline) deploy(ctx context.Context, d store.Deploy) {
	project, err := p.store.Project(ctx, d.ProjectID)
	if err != nil {
		p.log.Error("load project", "deploy", d.ID, "err", err)
		p.finish(ctx, project, d, err)
		return
	}
	logger := p.log.With("project", project.Name, "deploy", d.ID, "commit", shortSHA(d.CommitSHA))
	logger.Info("deploy started")
	p.report(ctx, project, d, github.StatePending, "Deploying")

	logFile, err := p.openLog(d.ID)
	if err != nil {
		p.finish(ctx, project, d, err)
		return
	}
	defer logFile.Close()

	err = p.execute(ctx, project, d, logFile)
	switch {
	case errors.Is(err, errSuperseded):
		logger.Info("deploy superseded", "err", err)
	case err != nil:
		_, _ = fmt.Fprintf(logFile, "\ndeploy failed: %v\n", err)
		logger.Warn("deploy failed", "err", err)
	default:
		logger.Info("deploy live")
	}
	p.finish(ctx, project, d, err)
}

// finish records a deploy's outcome. It ignores cancellation of ctx, so a
// deploy cut short by shutdown is still recorded instead of left mid-pipeline.
func (p *Pipeline) finish(ctx context.Context, project store.Project, d store.Deploy, deployErr error) {
	ctx = context.WithoutCancel(ctx)

	status, state, description, reason := store.StatusLive, github.StateSuccess, "Live", ""
	switch {
	case errors.Is(deployErr, errSuperseded):
		status, state, description, reason = store.StatusSuperseded, github.StateSuccess, "Superseded", deployErr.Error()
	case deployErr != nil:
		status, state, description, reason = store.StatusFailed, github.StateFailure, "Deploy failed", deployErr.Error()
	}
	if err := p.store.FinishDeploy(ctx, d.ID, status, reason); err != nil {
		p.log.Error("record deploy outcome", "deploy", d.ID, "err", err)
	}
	if project.ID != 0 {
		p.report(ctx, project, d, state, description)
	}
}

func (p *Pipeline) execute(ctx context.Context, project store.Project, d store.Deploy, log io.Writer) error {
	var built store.Release
	if d.Kind == store.KindPush {
		var err error
		if built, err = p.build(ctx, project, d, log); err != nil {
			return err
		}
	}

	p.activation.Lock()
	defer p.activation.Unlock()

	// Another deploy may have gone live while this one built or waited.
	project, err := p.store.Project(ctx, project.ID)
	if err != nil {
		return err
	}
	release, err := p.prepareRelease(ctx, project, d, built)
	if err != nil {
		return err
	}
	return p.activate(ctx, project, d, release, log)
}

// build turns a commit into an image that passed its tests. The result has no
// env vars yet: they are read just before it starts, so it gets the newest ones.
func (p *Pipeline) build(ctx context.Context, project store.Project, d store.Deploy, log io.Writer) (store.Release, error) {
	dir, err := os.MkdirTemp(filepath.Join(p.cfg.DataDir, "builds"), "deploy-*")
	if err != nil {
		return store.Release{}, fmt.Errorf("create build directory: %w", err)
	}
	defer os.RemoveAll(dir)

	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()

	if err := p.enter(ctx, d, store.StatusCloning, log); err != nil {
		return store.Release{}, err
	}
	repo := github.Repo{Owner: project.RepoOwner, Name: project.RepoName}
	if err := p.source.Fetch(buildCtx, repo, d.CommitSHA, dir, log); err != nil {
		return store.Release{}, err
	}

	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return store.Release{}, errors.New("the repo has no Dockerfile at its root")
	}
	file, err := shipfile.Load(dir)
	if err != nil {
		return store.Release{}, err
	}

	if err := p.enter(ctx, d, store.StatusBuilding, log); err != nil {
		return store.Release{}, err
	}
	image := imageTag(project, d)
	if err := p.runtime.Build(buildCtx, dir, image, log); err != nil {
		return store.Release{}, err
	}

	if file.Test.Command != "" {
		if err := p.enter(ctx, d, store.StatusTesting, log); err != nil {
			return store.Release{}, err
		}
		testCtx, cancelTest := context.WithTimeout(ctx, testTimeout)
		defer cancelTest()
		// Tests don't get the project's env vars, which are production secrets.
		if err := p.runtime.Test(testCtx, testContainerName(project, d), image, file.Test.Command, log); err != nil {
			p.removeImage(ctx, image)
			return store.Release{}, fmt.Errorf("tests failed: %w", err)
		}
	}

	return store.Release{
		ProjectID:  project.ID,
		CommitSHA:  d.CommitSHA,
		Image:      image,
		HealthPath: file.Health.Path,
		Memory:     file.Resources.Memory,
		CPUs:       file.Resources.CPUs,
	}, nil
}

// prepareRelease settles what d will run. This is where the three kinds of
// deploy differ; from here on they follow the same path (R1 to R3).
func (p *Pipeline) prepareRelease(ctx context.Context, project store.Project, d store.Deploy, built store.Release) (store.Release, error) {
	switch d.Kind {
	case store.KindPush:
		return p.newRelease(ctx, built)

	case store.KindEnv:
		if project.LiveDeployID == nil {
			return store.Release{}, errors.New("nothing is live to apply env vars to")
		}
		live, err := p.store.Deploy(ctx, *project.LiveDeployID)
		if err != nil {
			return store.Release{}, err
		}
		if live.ReleaseID == nil {
			return store.Release{}, errors.New("the live deploy has no release")
		}
		base, err := p.store.Release(ctx, *live.ReleaseID)
		if err != nil {
			return store.Release{}, err
		}
		return p.newRelease(ctx, base)

	case store.KindRollback:
		// Rolling back something already replaced would undo the replacement.
		if project.LiveDeployID == nil || d.PreviousDeployID == nil || *project.LiveDeployID != *d.PreviousDeployID {
			return store.Release{}, fmt.Errorf("%w: the deploy it was rolling back is no longer live", errSuperseded)
		}
		if d.ReleaseID == nil {
			return store.Release{}, errors.New("the release to roll back to no longer exists")
		}
		return p.store.Release(ctx, *d.ReleaseID)
	}
	return store.Release{}, fmt.Errorf("unknown deploy kind %q", d.Kind)
}

// newRelease saves r as a new release using the project's current env vars.
func (p *Pipeline) newRelease(ctx context.Context, r store.Release) (store.Release, error) {
	env, err := p.store.Env(ctx, r.ProjectID)
	if err != nil {
		return store.Release{}, err
	}
	r.Env = env
	return p.store.CreateRelease(ctx, r)
}

// activate is the one path to going live: start the release alongside the
// current version, wait for it to pass its health check, then move traffic. The
// current version isn't touched until traffic has moved, so any failure here
// leaves it serving (D4, D5).
func (p *Pipeline) activate(ctx context.Context, project store.Project, d store.Deploy, release store.Release, log io.Writer) error {
	name := containerName(project, d)
	if err := p.store.AssignRelease(ctx, d.ID, release, name, project.LiveDeployID); err != nil {
		return err
	}
	d.PreviousDeployID = project.LiveDeployID

	if err := p.enter(ctx, d, store.StatusStarting, log); err != nil {
		return err
	}
	if err := p.start(ctx, project, d, release, name); err != nil {
		p.removeContainer(ctx, name)
		return err
	}

	if err := p.enter(ctx, d, store.StatusHealthChecking, log); err != nil {
		p.removeContainer(ctx, name)
		return err
	}
	host := project.Name + "." + p.cfg.Domain
	if err := p.waitHealthy(ctx, name, host, release.HealthPath); err != nil {
		p.captureLogs(ctx, name, log)
		p.removeContainer(ctx, name)
		return err
	}

	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := p.switchTraffic(stepCtx, project, d); err != nil {
		p.removeContainer(ctx, name)
		return err
	}

	// Traffic has moved, so the deploy has succeeded whatever happens from here.
	select {
	case <-ctx.Done():
	case <-time.After(p.cfg.Timing.Drain):
	}
	p.retire(context.WithoutCancel(ctx), project, d)
	return nil
}

func (p *Pipeline) start(ctx context.Context, project store.Project, d store.Deploy, release store.Release, name string) error {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	env := make(map[string]string, len(release.Env)+1)
	for k, v := range release.Env {
		env[k] = v
	}
	env["PORT"] = strconv.Itoa(caddy.AppPort)

	return p.runtime.Start(ctx, docker.Container{
		Name:    name,
		Image:   release.Image,
		Network: p.cfg.AppsNetwork,
		Env:     env,
		Labels: map[string]string{
			"shipit.project": project.Name,
			"shipit.deploy":  strconv.FormatInt(d.ID, 10),
		},
		Memory: release.Memory,
		CPUs:   release.CPUs,
	})
}

// retire finishes replacing the previous deploy once traffic has left it. It is
// safe to run twice, because Recover may repeat it after a restart.
func (p *Pipeline) retire(ctx context.Context, project store.Project, d store.Deploy) {
	if d.PreviousDeployID == nil {
		return
	}
	previous, err := p.store.Deploy(ctx, *d.PreviousDeployID)
	if err != nil {
		p.log.Warn("load previous deploy", "deploy", *d.PreviousDeployID, "err", err)
		return
	}
	p.removeContainer(ctx, previous.Container)
	if d.Kind != store.KindRollback {
		return
	}

	// The restored release brings its env vars back too, so the settings match
	// what runs and the next push doesn't reintroduce a bad value.
	if d.ReleaseID != nil {
		release, err := p.store.Release(ctx, *d.ReleaseID)
		if err == nil {
			err = p.store.SetEnv(ctx, project.ID, release.Env)
		}
		if err != nil {
			p.log.Error("restore env vars", "deploy", d.ID, "err", err)
		}
	}
	if err := p.store.FinishDeploy(ctx, previous.ID, store.StatusRolledBack, d.CommitMessage); err != nil {
		p.log.Error("mark deploy rolled back", "deploy", previous.ID, "err", err)
	}
	p.report(ctx, project, previous, github.StateFailure, "Rolled back")
}

// switchTraffic points the project at d. If Caddy refuses, the database goes
// back to the previous deploy, so the two keep agreeing on what is live.
func (p *Pipeline) switchTraffic(ctx context.Context, project store.Project, d store.Deploy) error {
	if err := p.store.SetLiveDeploy(ctx, project.ID, &d.ID); err != nil {
		return err
	}
	err := p.router.Sync(ctx)
	if err == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if restoreErr := p.store.SetLiveDeploy(ctx, project.ID, project.LiveDeployID); restoreErr != nil {
		return errors.Join(err, restoreErr)
	}
	return err
}

// removeContainer is best effort: it runs on paths that already have an outcome.
func (p *Pipeline) removeContainer(ctx context.Context, name string) {
	if err := p.runtime.Remove(context.WithoutCancel(ctx), name); err != nil {
		p.log.Warn("remove container", "container", name, "err", err)
	}
}

// removeImage is best effort, like removeContainer.
func (p *Pipeline) removeImage(ctx context.Context, image string) {
	if err := p.runtime.RemoveImage(context.WithoutCancel(ctx), image); err != nil {
		p.log.Warn("remove image", "image", image, "err", err)
	}
}

// captureLogs saves what a failed app printed, which is usually the actual cause.
func (p *Pipeline) captureLogs(ctx context.Context, name string, log io.Writer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	_, _ = fmt.Fprintf(log, "==> app output\n")
	if err := p.runtime.Logs(ctx, name, failureLogLines, log); err != nil {
		p.log.Warn("read app logs", "container", name, "err", err)
	}
}

// enter records a stage. A cancelled ctx stops the deploy here, and finish then
// records it as failed.
func (p *Pipeline) enter(ctx context.Context, d store.Deploy, status store.Status, log io.Writer) error {
	if err := p.store.SetStage(ctx, d.ID, status); err != nil {
		return fmt.Errorf("record stage %s: %w", status, err)
	}
	_, _ = fmt.Fprintf(log, "==> %s\n", status)
	return nil
}

// report posts a commit status. GitHub being down must not fail a deploy. Only
// pushes report: env changes and rollbacks aren't about the commit they run.
func (p *Pipeline) report(ctx context.Context, project store.Project, d store.Deploy, state, description string) {
	if d.Kind != store.KindPush {
		return
	}
	repo := github.Repo{Owner: project.RepoOwner, Name: project.RepoName}
	target := "https://" + project.Name + "." + p.cfg.Domain
	if err := p.source.SetStatus(ctx, repo, d.CommitSHA, state, description, target); err != nil {
		p.log.Warn("post commit status", "deploy", d.ID, "err", err)
	}
}

func (p *Pipeline) openLog(id int64) (*os.File, error) {
	path := p.LogPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open deploy log: %w", err)
	}
	return f, nil
}

func containerName(project store.Project, d store.Deploy) string {
	return fmt.Sprintf("shipit-%s-%d", project.Name, d.ID)
}

func testContainerName(project store.Project, d store.Deploy) string {
	return containerName(project, d) + "-test"
}

func imageTag(project store.Project, d store.Deploy) string {
	return fmt.Sprintf("shipit/%s:%d-%s", project.Name, d.ID, shortSHA(d.CommitSHA))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
