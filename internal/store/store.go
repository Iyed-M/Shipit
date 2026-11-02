// Package store persists projects and deploys in SQLite.
package store

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a project or deploy doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a project name is already taken.
var ErrConflict = errors.New("already exists")

// Status is a deploy's position in the pipeline.
type Status string

// Deploy statuses, in pipeline order, followed by the other final states.
const (
	StatusQueued         Status = "queued"
	StatusCloning        Status = "cloning"
	StatusBuilding       Status = "building"
	StatusTesting        Status = "testing"
	StatusStarting       Status = "starting"
	StatusHealthChecking Status = "health-checking"
	StatusLive           Status = "live"
	StatusFailed         Status = "failed"
	StatusSuperseded     Status = "superseded"
	StatusRolledBack     Status = "rolled back"
)

// Kind says what started a deploy. All kinds end on the same path: a release is
// started, health-checked and given traffic.
type Kind string

// Deploy kinds.
const (
	// KindPush builds a commit into a new release.
	KindPush Kind = "push"
	// KindEnv makes a new release from the live image and the current env vars.
	KindEnv Kind = "env"
	// KindRollback brings back an earlier release, with its own env vars.
	KindRollback Kind = "rollback"
)

// Lane is a class of queued deploys, worked through independently so a long
// build can't hold up a rollback.
type Lane int

// Queue lanes.
const (
	LaneBuild Lane = iota
	LaneRelease
)

func (l Lane) where() string {
	if l == LaneBuild {
		return `kind = 'push'`
	}
	return `kind <> 'push'`
}

// Project is a GitHub repo deployed to its own address.
type Project struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	RepoOwner     string    `json:"repo_owner"`
	RepoName      string    `json:"repo_name"`
	Branch        string    `json:"branch"`
	WebhookID     int64     `json:"webhook_id"`
	WebhookSecret string    `json:"-"`
	LiveDeployID  *int64    `json:"live_deploy_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// Deploy is one attempt to take a commit live.
type Deploy struct {
	ID            int64  `json:"id"`
	ProjectID     int64  `json:"project_id"`
	Kind          Kind   `json:"kind"`
	CommitSHA     string `json:"commit_sha"`
	CommitMessage string `json:"commit_message"`
	Status        Status `json:"status"`
	Error         string `json:"error,omitempty"`
	Image         string `json:"image,omitempty"`
	Container     string `json:"container,omitempty"`
	ReleaseID     *int64 `json:"release_id,omitempty"`
	// PreviousDeployID is the deploy this one replaces when it goes live.
	PreviousDeployID *int64     `json:"previous_deploy_id,omitempty"`
	Restarts         int        `json:"restarts"`
	CreatedAt        time.Time  `json:"created_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

// NewDeploy is a deploy to queue.
type NewDeploy struct {
	ProjectID int64
	Kind      Kind
	CommitSHA string
	Message   string
	// Key makes queuing idempotent: a key already used queues nothing. It is
	// optional.
	Key string
	// ReleaseID is the release a rollback restores.
	ReleaseID *int64
	// PreviousDeployID is the deploy a rollback is meant to replace.
	PreviousDeployID *int64
}

// Stage records when a deploy entered a status.
type Stage struct {
	Status    Status    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// Route is what the router needs to know about a project.
type Route struct {
	Project string
	// Container is empty until the project's first deploy goes live.
	Container string
}

// Store is the SQLite database.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
}

// Open opens the database at path and applies pending migrations. masterKey
// must be 32 bytes; it seals env vars, which are never stored in the clear.
func Open(path string, masterKey []byte) (*Store, error) {
	aead, err := newCipher(masterKey)
	if err != nil {
		return nil, err
	}

	dsn := (&url.URL{
		Scheme: "file",
		Opaque: path,
		RawQuery: url.Values{"_pragma": {
			"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)",
		}}.Encode(),
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection keeps writers from racing each other for the lock.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(entries); i++ {
		name := entries[i].Name()
		script, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(script)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

const projectColumns = `id, name, repo_owner, repo_name, branch, webhook_id, webhook_secret, live_deploy_id, created_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var (
		p       Project
		created int64
	)
	err := row.Scan(&p.ID, &p.Name, &p.RepoOwner, &p.RepoName, &p.Branch,
		&p.WebhookID, &p.WebhookSecret, &p.LiveDeployID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	p.CreatedAt = time.UnixMilli(created).UTC()
	return p, nil
}

// CreateProject inserts p and returns it with its ID. A taken name gives ErrConflict.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	created, err := scanProject(s.db.QueryRowContext(ctx, `
		INSERT INTO projects (name, repo_owner, repo_name, branch, webhook_secret, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO NOTHING
		RETURNING `+projectColumns,
		p.Name, p.RepoOwner, p.RepoName, p.Branch, p.WebhookSecret, now()))
	if errors.Is(err, ErrNotFound) {
		return Project{}, ErrConflict
	}
	return created, err
}

// DeleteProject removes a project and its deploys.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

// SetWebhookID records the GitHub webhook created for a project.
func (s *Store) SetWebhookID(ctx context.Context, projectID, webhookID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET webhook_id = ? WHERE id = ?`, webhookID, projectID)
	return err
}

// SetLiveDeploy points a project at the deploy receiving traffic. A nil id clears it.
func (s *Store) SetLiveDeploy(ctx context.Context, projectID int64, deployID *int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET live_deploy_id = ? WHERE id = ?`, deployID, projectID)
	return err
}

// ProjectByName looks a project up by name.
func (s *Store) ProjectByName(ctx context.Context, name string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE name = ?`, name))
}

// Project looks a project up by ID.
func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

// Projects lists all projects, oldest first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	projects := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// Routes lists every project with the container currently receiving its traffic.
func (s *Store) Routes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, COALESCE(d.container, '')
		FROM projects p
		LEFT JOIN deploys d ON d.id = p.live_deploy_id
		ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.Project, &r.Container); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

const deployColumns = `id, project_id, kind, commit_sha, commit_message, status, error, image, container,
	release_id, previous_deploy_id, restarts, created_at, finished_at`

func scanDeploy(row interface{ Scan(...any) error }) (Deploy, error) {
	var (
		d        Deploy
		created  int64
		finished sql.NullInt64
	)
	err := row.Scan(&d.ID, &d.ProjectID, &d.Kind, &d.CommitSHA, &d.CommitMessage, &d.Status,
		&d.Error, &d.Image, &d.Container, &d.ReleaseID, &d.PreviousDeployID, &d.Restarts, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Deploy{}, ErrNotFound
	}
	if err != nil {
		return Deploy{}, err
	}
	d.CreatedAt = time.UnixMilli(created).UTC()
	if finished.Valid {
		t := time.UnixMilli(finished.Int64).UTC()
		d.FinishedAt = &t
	}
	return d, nil
}

// EnqueueDeploy queues a deploy. It returns false if its key was already used.
func (s *Store) EnqueueDeploy(ctx context.Context, n NewDeploy) (Deploy, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deploy{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	at := now()
	d, err := scanDeploy(tx.QueryRowContext(ctx, `
		INSERT INTO deploys (project_id, kind, commit_sha, commit_message, delivery_id,
			release_id, previous_deploy_id, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (delivery_id) DO NOTHING
		RETURNING `+deployColumns,
		n.ProjectID, n.Kind, n.CommitSHA, n.Message, sql.NullString{String: n.Key, Valid: n.Key != ""},
		n.ReleaseID, n.PreviousDeployID, StatusQueued, at))
	if errors.Is(err, ErrNotFound) {
		return Deploy{}, false, nil
	}
	if err != nil {
		return Deploy{}, false, err
	}
	if err := insertStage(ctx, tx, d.ID, StatusQueued, at); err != nil {
		return Deploy{}, false, err
	}
	return d, true, tx.Commit()
}

// Deploy looks a deploy up by ID.
func (s *Store) Deploy(ctx context.Context, id int64) (Deploy, error) {
	return scanDeploy(s.db.QueryRowContext(ctx,
		`SELECT `+deployColumns+` FROM deploys WHERE id = ?`, id))
}

// Deploys lists a project's most recent deploys, newest first.
func (s *Store) Deploys(ctx context.Context, projectID int64, limit int) ([]Deploy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deployColumns+` FROM deploys WHERE project_id = ? ORDER BY id DESC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	deploys := []Deploy{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		deploys = append(deploys, d)
	}
	return deploys, rows.Err()
}

// NextQueued returns a lane's oldest queued deploy, or ErrNotFound if none is waiting.
func (s *Store) NextQueued(ctx context.Context, lane Lane) (Deploy, error) {
	return scanDeploy(s.db.QueryRowContext(ctx,
		`SELECT `+deployColumns+` FROM deploys WHERE status = ? AND `+lane.where()+` ORDER BY id LIMIT 1`,
		StatusQueued))
}

// SupersededDeploys returns a lane's queued deploys that a newer queued deploy
// of the same project and kind makes pointless. Rollbacks each name their own
// target, so they are never superseded.
func (s *Store) SupersededDeploys(ctx context.Context, lane Lane) ([]Deploy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+deployColumns+` FROM deploys
		WHERE status = ? AND `+lane.where()+` AND kind <> ? AND id < (
			SELECT MAX(n.id) FROM deploys n
			WHERE n.project_id = deploys.project_id AND n.kind = deploys.kind AND n.status = ?)
		ORDER BY id`, StatusQueued, KindRollback, StatusQueued)
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

// SetStage moves a deploy to a pipeline status and records when it got there.
func (s *Store) SetStage(ctx context.Context, id int64, status Status) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	at := now()
	if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = ? WHERE id = ?`, status, id); err != nil {
		return err
	}
	if err := insertStage(ctx, tx, id, status, at); err != nil {
		return err
	}
	return tx.Commit()
}

// AssignRelease records what a deploy is about to run and what it replaces.
func (s *Store) AssignRelease(ctx context.Context, id int64, r Release, container string, previous *int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deploys SET release_id = ?, image = ?, commit_sha = ?, container = ?, previous_deploy_id = ?
		WHERE id = ?`, r.ID, r.Image, r.CommitSHA, container, previous, id)
	return err
}

// FinishDeploy moves a deploy to a final status.
func (s *Store) FinishDeploy(ctx context.Context, id int64, status Status, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	at := now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE deploys SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, reason, at, id); err != nil {
		return err
	}
	if err := insertStage(ctx, tx, id, status, at); err != nil {
		return err
	}
	return tx.Commit()
}

// InFlightDeploys returns deploys that were mid-pipeline. At startup these
// belong to a previous process, which can no longer finish them.
func (s *Store) InFlightDeploys(ctx context.Context) ([]Deploy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deployColumns+` FROM deploys WHERE status IN (?, ?, ?, ?, ?) ORDER BY id`,
		StatusCloning, StatusBuilding, StatusTesting, StatusStarting, StatusHealthChecking)
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

// Stages returns a deploy's stage history in order.
func (s *Store) Stages(ctx context.Context, deployID int64) ([]Stage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, started_at FROM deploy_stages WHERE deploy_id = ? ORDER BY rowid`, deployID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stages := []Stage{}
	for rows.Next() {
		var (
			st Stage
			at int64
		)
		if err := rows.Scan(&st.Status, &at); err != nil {
			return nil, err
		}
		st.StartedAt = time.UnixMilli(at).UTC()
		stages = append(stages, st)
	}
	return stages, rows.Err()
}

func insertStage(ctx context.Context, tx *sql.Tx, id int64, status Status, at int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deploy_stages (deploy_id, status, started_at) VALUES (?, ?, ?)`, id, status, at)
	return err
}

func now() int64 {
	return time.Now().UnixMilli()
}
