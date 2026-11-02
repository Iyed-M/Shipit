-- A release is an image plus everything needed to run it again without the
-- repo: a sealed copy of its env vars, its health check and its limits.
CREATE TABLE releases (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    commit_sha  TEXT    NOT NULL,
    image       TEXT    NOT NULL,
    env         BLOB,
    health_path TEXT    NOT NULL DEFAULT '',
    memory      TEXT    NOT NULL,
    cpus        REAL    NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE INDEX releases_project ON releases (project_id, id DESC);

-- The env vars the next release will use, sealed.
ALTER TABLE projects ADD COLUMN env BLOB;

ALTER TABLE deploys ADD COLUMN kind TEXT NOT NULL DEFAULT 'push';
ALTER TABLE deploys ADD COLUMN release_id INTEGER REFERENCES releases (id) ON DELETE SET NULL;
-- The deploy this one replaces when it goes live.
ALTER TABLE deploys ADD COLUMN previous_deploy_id INTEGER REFERENCES deploys (id) ON DELETE SET NULL;
-- Docker's restart count at the last check, so crashes are counted once, even
-- across a Shipit restart.
ALTER TABLE deploys ADD COLUMN restarts INTEGER NOT NULL DEFAULT 0;

CREATE TABLE crashes (
    deploy_id  INTEGER NOT NULL REFERENCES deploys (id) ON DELETE CASCADE,
    crashed_at INTEGER NOT NULL
);

CREATE INDEX crashes_deploy ON crashes (deploy_id, crashed_at);
