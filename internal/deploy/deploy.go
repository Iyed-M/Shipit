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

	interruptedReason = "interrupted by a Shipit restart"
)

// Runtime runs images. It is the seam that lets tests use a fake Docker.
type Runtime interface {
	Build(ctx context.Context, dir, tag string, log io.Writer) error
	Test(ctx context.Context, name, image, command string, log io.Writer) error
	Start(ctx context.Context, c docker.Container) error
	Remove(ctx context.Context, name string) error
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
}

// Pipeline runs deploys one at a time.
type Pipeline struct {
	cfg     Config
	store   *store.Store
	runtime Runtime
	source  Source
	router  Router
	log     *slog.Logger
	wake    chan struct{}
}

// NewPipeline returns a Pipeline using the given collaborators.
func NewPipeline(cfg Config, s *store.Store, r Runtime, src Source, router Router, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg:     cfg,
		store:   s,
		runtime: r,
		source:  src,
		router:  router,
		log:     log,
		wake:    make(chan struct{}, 1),
	}
}

// Wake tells the worker that a deploy has been queued.
func (p *Pipeline) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run works through the queue until ctx ends. Only one deploy runs at a time,
// across all projects, so builds never compete for the server.
func (p *Pipeline) Run(ctx context.Context) {
	for {
		p.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
	}
}

func (p *Pipeline) drain(ctx context.Context) {
	for ctx.Err() == nil {
		d, err := p.store.NextQueued(ctx, store.LaneBuild)
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

// Recover fails deploys left mid-pipeline by a previous process. The version
// that was serving is untouched, so all that remains is to clear up the deploy.
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
	if err != nil {
		_, _ = fmt.Fprintf(logFile, "\ndeploy failed: %v\n", err)
		logger.Warn("deploy failed", "err", err)
	} else {
		logger.Info("deploy live")
	}
	p.finish(ctx, project, d, err)
}

// finish records a deploy's outcome. It ignores cancellation of ctx, so a
// deploy cut short by shutdown is still recorded instead of left mid-pipeline.
func (p *Pipeline) finish(ctx context.Context, project store.Project, d store.Deploy, deployErr error) {
	ctx = context.WithoutCancel(ctx)

	status, state, description, reason := store.StatusLive, github.StateSuccess, "Live", ""
	if deployErr != nil {
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
	dir, err := os.MkdirTemp(filepath.Join(p.cfg.DataDir, "builds"), "deploy-*")
	if err != nil {
		return fmt.Errorf("create build directory: %w", err)
	}
	defer os.RemoveAll(dir)

	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()

	if err := p.enter(ctx, d, store.StatusCloning, log); err != nil {
		return err
	}
	repo := github.Repo{Owner: project.RepoOwner, Name: project.RepoName}
	if err := p.source.Fetch(buildCtx, repo, d.CommitSHA, dir, log); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return errors.New("the repo has no Dockerfile at its root")
	}
	file, err := shipfile.Load(dir)
	if err != nil {
		return err
	}

	if err := p.enter(ctx, d, store.StatusBuilding, log); err != nil {
		return err
	}
	image := imageTag(project, d)
	if err := p.runtime.Build(buildCtx, dir, image, log); err != nil {
		return err
	}

	if file.Test.Command != "" {
		if err := p.enter(ctx, d, store.StatusTesting, log); err != nil {
			return err
		}
		testCtx, cancelTest := context.WithTimeout(ctx, testTimeout)
		defer cancelTest()
		if err := p.runtime.Test(testCtx, testContainerName(project, d), image, file.Test.Command, log); err != nil {
			return fmt.Errorf("tests failed: %w", err)
		}
	}

	if err := p.enter(ctx, d, store.StatusStarting, log); err != nil {
		return err
	}
	return p.release(ctx, project, d, image, file)
}

// release starts the new container, moves traffic to it and removes the old one.
func (p *Pipeline) release(ctx context.Context, project store.Project, d store.Deploy, image string, file shipfile.File) error {
	name := containerName(project, d)
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	if err := p.store.SetArtifacts(ctx, d.ID, image, name); err != nil {
		return err
	}
	err := p.runtime.Start(ctx, docker.Container{
		Name:    name,
		Image:   image,
		Network: p.cfg.AppsNetwork,
		Env:     map[string]string{"PORT": strconv.Itoa(caddy.AppPort)},
		Labels: map[string]string{
			"shipit.project": project.Name,
			"shipit.deploy":  strconv.FormatInt(d.ID, 10),
		},
		Memory: file.Resources.Memory,
		CPUs:   file.Resources.CPUs,
	})
	if err != nil {
		p.removeContainer(ctx, name)
		return err
	}

	if err := p.switchTraffic(ctx, project, d); err != nil {
		p.removeContainer(ctx, name)
		return err
	}

	if project.LiveDeployID != nil {
		previous, err := p.store.Deploy(ctx, *project.LiveDeployID)
		if err != nil {
			p.log.Warn("load previous deploy", "deploy", *project.LiveDeployID, "err", err)
			return nil
		}
		p.removeContainer(ctx, previous.Container)
	}
	return nil
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

// enter records a stage. A cancelled ctx stops the deploy here, and finish then
// records it as failed.
func (p *Pipeline) enter(ctx context.Context, d store.Deploy, status store.Status, log io.Writer) error {
	if err := p.store.SetStage(ctx, d.ID, status); err != nil {
		return fmt.Errorf("record stage %s: %w", status, err)
	}
	_, _ = fmt.Fprintf(log, "==> %s\n", status)
	return nil
}

// report posts a commit status. GitHub being down must not fail a deploy.
func (p *Pipeline) report(ctx context.Context, project store.Project, d store.Deploy, state, description string) {
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
