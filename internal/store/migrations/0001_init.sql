CREATE TABLE projects (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    repo_owner     TEXT    NOT NULL,
    repo_name      TEXT    NOT NULL,
    branch         TEXT    NOT NULL,
    webhook_id     INTEGER NOT NULL DEFAULT 0,
    webhook_secret TEXT    NOT NULL,
    live_deploy_id INTEGER REFERENCES deploys (id),
    created_at     INTEGER NOT NULL
);

CREATE TABLE deploys (
    id             INTEGER PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    commit_sha     TEXT    NOT NULL,
    commit_message TEXT    NOT NULL DEFAULT '',
    -- GitHub's delivery id. Unique so a redelivered webhook is ignored.
    delivery_id    TEXT UNIQUE,
    status         TEXT    NOT NULL,
    error          TEXT    NOT NULL DEFAULT '',
    image          TEXT    NOT NULL DEFAULT '',
    container      TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    finished_at    INTEGER
);

CREATE INDEX deploys_project ON deploys (project_id, id DESC);
CREATE INDEX deploys_status ON deploys (status);

CREATE TABLE deploy_stages (
    deploy_id  INTEGER NOT NULL REFERENCES deploys (id) ON DELETE CASCADE,
    status     TEXT    NOT NULL,
    started_at INTEGER NOT NULL
);

CREATE INDEX deploy_stages_deploy ON deploy_stages (deploy_id);
