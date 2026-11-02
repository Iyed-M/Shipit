package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Release is an image plus a saved copy of its env vars, health check and
// limits. Starting it needs nothing from the repo, which is what lets env
// changes and rollbacks skip the build.
type Release struct {
	ID         int64             `json:"id"`
	ProjectID  int64             `json:"project_id"`
	CommitSHA  string            `json:"commit_sha"`
	Image      string            `json:"image"`
	Env        map[string]string `json:"-"`
	HealthPath string            `json:"health_path,omitempty"`
	Memory     string            `json:"memory"`
	CPUs       float64           `json:"cpus"`
	CreatedAt  time.Time         `json:"created_at"`
}

// Crash records an app exiting on its own while it was live.
type Crash struct {
	DeployID  int64     `json:"deploy_id"`
	CrashedAt time.Time `json:"crashed_at"`
}

const releaseColumns = `id, project_id, commit_sha, image, env, health_path, memory, cpus, created_at`

// wentLive is true for releases that passed their health check at least once.
const wentLive = `EXISTS (
	SELECT 1 FROM deploys d
	WHERE d.release_id = releases.id AND d.status IN ('live', 'rolled back'))`

func (s *Store) scanRelease(row interface{ Scan(...any) error }) (Release, error) {
	var (
		r       Release
		sealed  []byte
		created int64
	)
	err := row.Scan(&r.ID, &r.ProjectID, &r.CommitSHA, &r.Image, &sealed, &r.HealthPath, &r.Memory, &r.CPUs, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	if err != nil {
		return Release{}, err
	}
	if r.Env, err = s.open(r.ProjectID, sealed); err != nil {
		return Release{}, err
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	return r, nil
}

// CreateRelease saves r and returns it with its ID.
func (s *Store) CreateRelease(ctx context.Context, r Release) (Release, error) {
	sealed, err := s.seal(r.ProjectID, r.Env)
	if err != nil {
		return Release{}, err
	}
	return s.scanRelease(s.db.QueryRowContext(ctx, `
		INSERT INTO releases (project_id, commit_sha, image, env, health_path, memory, cpus, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING `+releaseColumns,
		r.ProjectID, r.CommitSHA, r.Image, sealed, r.HealthPath, r.Memory, r.CPUs, now()))
}

// Release looks a release up by ID.
func (s *Store) Release(ctx context.Context, id int64) (Release, error) {
	return s.scanRelease(s.db.QueryRowContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE id = ?`, id))
}

// RollbackCandidate returns one of a project's releases that has been live. A
// release that never passed its health check gives ErrNotFound.
func (s *Store) RollbackCandidate(ctx context.Context, projectID, id int64) (Release, error) {
	return s.scanRelease(s.db.QueryRowContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE id = ? AND project_id = ? AND `+wentLive, id, projectID))
}

// Releases lists a project's releases that have been live, newest first.
func (s *Store) Releases(ctx context.Context, projectID int64, limit int) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE project_id = ? AND `+wentLive+` ORDER BY id DESC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	releases := []Release{}
	for rows.Next() {
		r, err := s.scanRelease(rows)
		if err != nil {
			return nil, err
		}
		releases = append(releases, r)
	}
	return releases, rows.Err()
}

// StaleReleases returns the releases whose image can be deleted. It keeps the
// newest keep images that have been live, and always the one serving now, so
// a rollback target's image is still there. Releases that never went live don't
// count towards keep, so failed deploys can't push good images out.
func (s *Store) StaleReleases(ctx context.Context, projectID int64, keep int) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, image, `+wentLive+`,
			id = (SELECT d.release_id FROM deploys d
				JOIN projects p ON p.live_deploy_id = d.id WHERE p.id = ?)
		FROM releases WHERE project_id = ? ORDER BY id DESC`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type entry struct {
		id    int64
		image string
	}
	var entries []entry
	kept := map[string]bool{}
	newest := 0
	for rows.Next() {
		var (
			e       entry
			live    bool
			serving sql.NullBool
		)
		if err := rows.Scan(&e.id, &e.image, &live, &serving); err != nil {
			return nil, err
		}
		entries = append(entries, e)
		if live && !kept[e.image] && newest < keep {
			kept[e.image] = true
			newest++
		}
		if serving.Bool {
			kept[e.image] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stale := []Release{}
	for _, e := range entries {
		if !kept[e.image] {
			stale = append(stale, Release{ID: e.id, ProjectID: projectID, Image: e.image})
		}
	}
	return stale, nil
}

// DeleteRelease removes a release whose image is gone. Deploys keep their
// history but no longer point at it.
func (s *Store) DeleteRelease(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id = ?`, id)
	return err
}

// LiveDeploys returns the deploy currently serving each project. A deploy still
// switching traffic isn't included until it has finished going live.
func (s *Store) LiveDeploys(ctx context.Context) ([]Deploy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deployColumns+` FROM deploys
		WHERE status = ? AND id IN (SELECT live_deploy_id FROM projects) ORDER BY id`, StatusLive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deploys []Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		deploys = append(deploys, d)
	}
	return deploys, rows.Err()
}

// RecordCrashes saves count crashes of a deploy and the restart count they
// bring it to.
func (s *Store) RecordCrashes(ctx context.Context, deployID int64, count, restarts int, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for range count {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO crashes (deploy_id, crashed_at) VALUES (?, ?)`, deployID, at.UnixMilli()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deploys SET restarts = ? WHERE id = ?`, restarts, deployID); err != nil {
		return err
	}
	return tx.Commit()
}

// CountCrashes counts a deploy's crashes up to and including until.
func (s *Store) CountCrashes(ctx context.Context, deployID int64, until time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crashes WHERE deploy_id = ? AND crashed_at <= ?`,
		deployID, until.UnixMilli()).Scan(&n)
	return n, err
}

// Crashes lists a project's most recent crashes, newest first.
func (s *Store) Crashes(ctx context.Context, projectID int64, limit int) ([]Crash, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.deploy_id, c.crashed_at FROM crashes c
		JOIN deploys d ON d.id = c.deploy_id
		WHERE d.project_id = ?
		ORDER BY c.crashed_at DESC, c.rowid DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	crashes := []Crash{}
	for rows.Next() {
		var (
			c  Crash
			at int64
		)
		if err := rows.Scan(&c.DeployID, &at); err != nil {
			return nil, err
		}
		c.CrashedAt = time.UnixMilli(at).UTC()
		crashes = append(crashes, c)
	}
	return crashes, rows.Err()
}
