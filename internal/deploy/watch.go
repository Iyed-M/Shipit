package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	// crashLimit crashes within crashWindow of going live means the release is
	// bad, so the previous one comes back.
	crashLimit  = 3
	crashWindow = 2 * time.Minute
)

// ErrNoRollbackTarget means there is no earlier release to go back to.
var ErrNoRollbackTarget = errors.New("no earlier release to roll back to")

// watch records crashes of live containers until ctx ends. Docker does the
// restarting; this notices it happened, by comparing Docker's restart count with
// the saved one. Crashes during a Shipit outage are therefore still counted.
func (p *Pipeline) watch(ctx context.Context) {
	for {
		p.checkCrashes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.cfg.Timing.WatchInterval):
		}
	}
}

func (p *Pipeline) checkCrashes(ctx context.Context) {
	deploys, err := p.store.LiveDeploys(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Error("list live deploys", "err", err)
		}
		return
	}
	for _, d := range deploys {
		state, err := p.runtime.Inspect(ctx, d.Container)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("inspect live container", "container", d.Container, "err", err)
			}
			continue
		}
		crashes := state.RestartCount - d.Restarts
		if crashes <= 0 {
			continue
		}
		if err := p.store.RecordCrashes(ctx, d.ID, crashes, state.RestartCount, state.FinishedAt); err != nil {
			p.log.Error("record crashes", "deploy", d.ID, "err", err)
			continue
		}
		p.log.Warn("app crashed", "container", d.Container, "crashes", state.RestartCount)
		p.rollBackIfCrashLooping(ctx, d)
	}
}

func (p *Pipeline) rollBackIfCrashLooping(ctx context.Context, d store.Deploy) {
	// A rollback that crashes has nothing better to go back to, and rolling
	// back again would bounce between two bad releases forever.
	if d.Kind == store.KindRollback || d.FinishedAt == nil {
		return
	}
	crashes, err := p.store.CountCrashes(ctx, d.ID, d.FinishedAt.Add(crashWindow))
	if err != nil {
		p.log.Error("count crashes", "deploy", d.ID, "err", err)
		return
	}
	if crashes < crashLimit {
		return
	}
	project, err := p.store.Project(ctx, d.ProjectID)
	if err != nil {
		p.log.Error("load project", "deploy", d.ID, "err", err)
		return
	}
	target, err := p.previousRelease(ctx, d)
	if err != nil {
		p.log.Warn("can't roll back crash-looping app", "project", project.Name, "err", err)
		return
	}
	reason := fmt.Sprintf("Automatic rollback: crashed %d times within %s of going live", crashes, crashWindow)
	// The key makes this happen once per bad deploy, however often it crashes.
	key := "rollback-" + strconv.FormatInt(d.ID, 10)
	if _, err := p.enqueueRollback(ctx, d, target, reason, key); err != nil {
		p.log.Error("queue rollback", "project", project.Name, "err", err)
		return
	}
	p.log.Warn("rolling back crash-looping app", "project", project.Name, "release", target.ID)
}

// Rollback queues a return to an earlier release. With no releaseID it uses the
// release that was live before the current one.
func (p *Pipeline) Rollback(ctx context.Context, project store.Project, releaseID *int64) (store.Deploy, error) {
	if project.LiveDeployID == nil {
		return store.Deploy{}, ErrNoRollbackTarget
	}
	live, err := p.store.Deploy(ctx, *project.LiveDeployID)
	if err != nil {
		return store.Deploy{}, err
	}

	var target store.Release
	if releaseID == nil {
		target, err = p.previousRelease(ctx, live)
	} else {
		target, err = p.store.RollbackCandidate(ctx, project.ID, *releaseID)
		if errors.Is(err, store.ErrNotFound) {
			err = ErrNoRollbackTarget
		}
	}
	if err != nil {
		return store.Deploy{}, err
	}
	if live.ReleaseID != nil && *live.ReleaseID == target.ID {
		return store.Deploy{}, fmt.Errorf("%w: release %d is already live", ErrNoRollbackTarget, target.ID)
	}
	return p.enqueueRollback(ctx, live, target, fmt.Sprintf("Rollback to release %d", target.ID), "")
}

func (p *Pipeline) previousRelease(ctx context.Context, d store.Deploy) (store.Release, error) {
	if d.PreviousDeployID == nil {
		return store.Release{}, ErrNoRollbackTarget
	}
	previous, err := p.store.Deploy(ctx, *d.PreviousDeployID)
	if err != nil {
		return store.Release{}, err
	}
	if previous.ReleaseID == nil {
		return store.Release{}, ErrNoRollbackTarget
	}
	return p.store.Release(ctx, *previous.ReleaseID)
}

func (p *Pipeline) enqueueRollback(ctx context.Context, from store.Deploy, target store.Release, message, key string) (store.Deploy, error) {
	d, _, err := p.store.EnqueueDeploy(ctx, store.NewDeploy{
		ProjectID:        from.ProjectID,
		Kind:             store.KindRollback,
		CommitSHA:        target.CommitSHA,
		Message:          message,
		Key:              key,
		ReleaseID:        &target.ID,
		PreviousDeployID: &from.ID,
	})
	if err != nil {
		return store.Deploy{}, err
	}
	p.Wake()
	return d, nil
}
