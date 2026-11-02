package store

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shipit.db")
	s, err := Open(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

func project(t *testing.T, s *Store, name string) Project {
	t.Helper()
	p, err := s.CreateProject(t.Context(), Project{
		Name: name, RepoOwner: "o", RepoName: name, Branch: "main", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func setEnv(t *testing.T, s *Store, id int64, name, value string) {
	t.Helper()
	if _, err := s.UpdateEnv(t.Context(), id, map[string]*string{name: &value}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsShortKey(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "shipit.db"), []byte("short")); err == nil {
		t.Error("expected an error")
	}
}

func TestEnvIsEncryptedOnDisk(t *testing.T) {
	s, path := open(t)
	p := project(t, s, "demo")
	setEnv(t, s, p.ID, "DATABASE_URL", "very-secret-value")
	if _, err := s.CreateRelease(t.Context(), Release{
		ProjectID: p.ID, Image: "i", Memory: "1m", CPUs: 1, Env: map[string]string{"KEY": "very-secret-value"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"very-secret-value", "DATABASE_URL"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s contains %s in the clear", f, secret)
			}
		}
	}
}

func TestEnvRoundTrip(t *testing.T) {
	s, _ := open(t)
	p := project(t, s, "demo")

	env, err := s.Env(t.Context(), p.ID)
	if err != nil || len(env) != 0 {
		t.Fatalf("empty env = %v, %v", env, err)
	}
	setEnv(t, s, p.ID, "A", "multi\nline=value")
	env, err = s.Env(t.Context(), p.ID)
	if err != nil || env["A"] != "multi\nline=value" {
		t.Errorf("env = %v, %v", env, err)
	}
}

func TestEnvNeedsTheRightKey(t *testing.T) {
	s, path := open(t)
	p := project(t, s, "demo")
	setEnv(t, s, p.ID, "A", "1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	other, err := Open(path, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Env(t.Context(), p.ID); err == nil {
		t.Error("opened env vars with the wrong key")
	}
}

// A sealed value copied onto another project must not open there.
func TestEnvIsBoundToItsProject(t *testing.T) {
	s, _ := open(t)
	a, b := project(t, s, "a"), project(t, s, "b")
	setEnv(t, s, a.ID, "A", "1")

	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE projects SET env = (SELECT env FROM projects WHERE id = ?) WHERE id = ?`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Env(t.Context(), b.ID); err == nil {
		t.Error("a sealed value opened for the wrong project")
	}
}

func TestEnvUnknownProject(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Env(t.Context(), 99); err != ErrNotFound {
		t.Errorf("err = %v", err)
	}
	if _, err := s.UpdateEnv(t.Context(), 99, nil); err != ErrNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestReleaseKeepsItsOwnEnv(t *testing.T) {
	s, _ := open(t)
	p := project(t, s, "demo")
	r, err := s.CreateRelease(t.Context(), Release{
		ProjectID: p.ID, Image: "i", HealthPath: "/up", Memory: "1g", CPUs: 2, Env: map[string]string{"A": "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, s, p.ID, "A", "new")

	got, err := s.Release(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["A"] != "old" || got.HealthPath != "/up" || got.Memory != "1g" || got.CPUs != 2 {
		t.Errorf("release = %+v", got)
	}
}

// seedRelease adds a release and a finished deploy of it.
func seedRelease(t *testing.T, s *Store, p Project, image string, status Status) (Release, Deploy) {
	t.Helper()
	ctx := t.Context()
	r, err := s.CreateRelease(ctx, Release{ProjectID: p.ID, Image: image, Memory: "1m", CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.EnqueueDeploy(ctx, NewDeploy{ProjectID: p.ID, Kind: KindPush})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AssignRelease(ctx, d.ID, r, "c", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeploy(ctx, d.ID, status, ""); err != nil {
		t.Fatal(err)
	}
	return r, d
}

func staleImages(t *testing.T, s *Store, id int64) []string {
	t.Helper()
	stale, err := s.StaleReleases(t.Context(), id, 2)
	if err != nil {
		t.Fatal(err)
	}
	images := []string{}
	for _, r := range stale {
		images = append(images, r.Image)
	}
	return images
}

func TestStaleReleases(t *testing.T) {
	s, _ := open(t)
	p := project(t, s, "demo")

	seedRelease(t, s, p, "a", StatusLive)
	seedRelease(t, s, p, "b", StatusRolledBack)
	seedRelease(t, s, p, "failed", StatusFailed)
	seedRelease(t, s, p, "c", StatusLive)

	if got := staleImages(t, s, p.ID); !slices.Equal(got, []string{"failed", "a"}) {
		t.Errorf("stale = %v", got)
	}
}

func TestStaleReleasesKeepsWhatIsServing(t *testing.T) {
	s, _ := open(t)
	p := project(t, s, "demo")

	// Traffic has moved to this deploy, but it hasn't finished going live yet,
	// which is when another deploy's cleanup could run.
	r, err := s.CreateRelease(t.Context(), Release{ProjectID: p.ID, Image: "serving", Memory: "1m", CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.EnqueueDeploy(t.Context(), NewDeploy{ProjectID: p.ID, Kind: KindPush})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AssignRelease(t.Context(), d.ID, r, "c", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLiveDeploy(t.Context(), p.ID, &d.ID); err != nil {
		t.Fatal(err)
	}
	seedRelease(t, s, p, "a", StatusLive)
	seedRelease(t, s, p, "b", StatusLive)
	seedRelease(t, s, p, "c", StatusLive)

	if got := staleImages(t, s, p.ID); !slices.Equal(got, []string{"a"}) {
		t.Errorf("stale = %v, want only a", got)
	}
}

func TestDeletedReleaseLeavesDeployHistory(t *testing.T) {
	s, _ := open(t)
	p := project(t, s, "demo")
	r, d := seedRelease(t, s, p, "a", StatusLive)

	if err := s.DeleteRelease(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Deploy(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReleaseID != nil || got.Image != "a" {
		t.Errorf("deploy = %+v", got)
	}
}

// Databases created by the first release have no release tables.
func TestMigratesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shipit.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(script),
		`PRAGMA user_version = 1`,
		`INSERT INTO projects (name, repo_owner, repo_name, branch, webhook_secret, created_at)
			VALUES ('demo', 'o', 'r', 'main', 's', 0)`,
		`INSERT INTO deploys (project_id, commit_sha, delivery_id, status, created_at)
			VALUES (1, 'abc', 'old', 'live', 0)`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	d, err := s.Deploy(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindPush || d.ReleaseID != nil {
		t.Errorf("deploy = %+v", d)
	}
}
