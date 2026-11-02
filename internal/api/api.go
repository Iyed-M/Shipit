// Package api serves Shipit's HTTP API and receives GitHub webhooks.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	// maxWebhookBytes matches the size GitHub caps webhook payloads at.
	maxWebhookBytes = 25 << 20
	maxBodyBytes    = 1 << 20
	deployListLimit = 50
	defaultBranch   = "main"
)

var projectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// GitHub is the part of the GitHub client the API needs.
type GitHub interface {
	CreateWebhook(ctx context.Context, repo github.Repo, target, secret string) (int64, error)
}

// Router publishes routes from the store.
type Router interface {
	Sync(ctx context.Context) error
}

// Queue is notified of new deploys and knows where their logs are.
type Queue interface {
	Wake()
	LogPath(id int64) string
}

// Config holds the API's settings.
type Config struct {
	AdminToken string
	// PublicURL is the origin GitHub sends webhooks to.
	PublicURL string
	Domain    string
}

// Server handles HTTP requests.
type Server struct {
	cfg    Config
	store  *store.Store
	github GitHub
	router Router
	queue  Queue
	log    *slog.Logger
}

// New returns a Server using the given collaborators.
func New(cfg Config, s *store.Store, gh GitHub, router Router, queue Queue, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: s, github: gh, router: router, queue: queue, log: log}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /webhooks/github/{project}", s.handleWebhook)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/projects", s.createProject)
	api.HandleFunc("GET /api/projects", s.listProjects)
	api.HandleFunc("GET /api/projects/{project}", s.getProject)
	api.HandleFunc("GET /api/projects/{project}/deploys", s.listDeploys)
	api.HandleFunc("GET /api/deploys/{id}", s.getDeploy)
	api.HandleFunc("GET /api/deploys/{id}/logs", s.getDeployLogs)
	mux.Handle("/api/", s.requireAdmin(api))

	return mux
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.AdminToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createProjectRequest struct {
	RepoURL string `json:"repo_url"`
	Name    string `json:"name"`
	Branch  string `json:"branch"`
}

type projectResponse struct {
	store.Project
	URL string `json:"url"`
}

func (s *Server) projectResponse(p store.Project) projectResponse {
	return projectResponse{Project: p, URL: "https://" + p.Name + "." + s.cfg.Domain}
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	repo, err := github.ParseRepoURL(req.RepoURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := req.Name
	if name == "" {
		name = slugify(repo.Name)
	}
	if !projectName.MatchString(name) {
		writeError(w, http.StatusBadRequest,
			"name must be lowercase letters, digits and hyphens, at most 63 characters")
		return
	}
	branch := req.Branch
	if branch == "" {
		branch = defaultBranch
	}
	secret, err := randomSecret()
	if err != nil {
		s.internalError(w, "generate webhook secret", err)
		return
	}

	project, err := s.store.CreateProject(r.Context(), store.Project{
		Name:          name,
		RepoOwner:     repo.Owner,
		RepoName:      repo.Name,
		Branch:        branch,
		WebhookSecret: secret,
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, fmt.Sprintf("project %q already exists", name))
		return
	}
	if err != nil {
		s.internalError(w, "create project", err)
		return
	}

	target := s.cfg.PublicURL + "/webhooks/github/" + project.Name
	webhookID, err := s.github.CreateWebhook(r.Context(), repo, target, secret)
	if err != nil {
		// Without a webhook the project can never deploy, so don't keep it.
		if delErr := s.store.DeleteProject(context.WithoutCancel(r.Context()), project.ID); delErr != nil {
			s.log.Error("remove project after webhook failure", "project", name, "err", delErr)
		}
		s.log.Warn("create webhook", "project", name, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.store.SetWebhookID(r.Context(), project.ID, webhookID); err != nil {
		s.internalError(w, "save webhook id", err)
		return
	}
	project.WebhookID = webhookID

	// A failure here heals on the next sync, so the project still counts as created.
	if err := s.router.Sync(r.Context()); err != nil {
		s.log.Error("publish route", "project", name, "err", err)
	}
	writeJSON(w, http.StatusCreated, s.projectResponse(project))
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.Projects(r.Context())
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	out := make([]projectResponse, 0, len(projects))
	for _, p := range projects {
		out = append(out, s.projectResponse(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	project, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.projectResponse(project))
}

func (s *Server) listDeploys(w http.ResponseWriter, r *http.Request) {
	project, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	deploys, err := s.store.Deploys(r.Context(), project.ID, deployListLimit)
	if err != nil {
		s.internalError(w, "list deploys", err)
		return
	}
	writeJSON(w, http.StatusOK, deploys)
}

func (s *Server) getDeploy(w http.ResponseWriter, r *http.Request) {
	deploy, ok := s.loadDeploy(w, r)
	if !ok {
		return
	}
	stages, err := s.store.Stages(r.Context(), deploy.ID)
	if err != nil {
		s.internalError(w, "list stages", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.Deploy
		Stages []store.Stage `json:"stages"`
	}{deploy, stages})
}

func (s *Server) getDeployLogs(w http.ResponseWriter, r *http.Request) {
	deploy, ok := s.loadDeploy(w, r)
	if !ok {
		return
	}
	file, err := os.Open(s.queue.LogPath(deploy.ID))
	if errors.Is(err, os.ErrNotExist) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return
	}
	if err != nil {
		s.internalError(w, "open deploy log", err)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, file)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.ProjectByName(r.Context(), r.PathValue("project"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown project")
		return
	}
	if err != nil {
		s.internalError(w, "load project", err)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if !github.VerifySignature(project.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// The signature proves GitHub sent it; other events, such as the ping sent
	// when a webhook is created, just aren't deploys.
	if r.Header.Get("X-GitHub-Event") != "push" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		writeError(w, http.StatusBadRequest, "missing delivery id")
		return
	}
	push, err := github.ParsePush(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if push.Deleted || push.Branch() != project.Branch {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	deploy, created, err := s.store.EnqueueDeploy(r.Context(), store.NewDeploy{
		ProjectID: project.ID,
		Kind:      store.KindPush,
		CommitSHA: push.After,
		Message:   firstLine(push.HeadCommit.Message),
		Key:       delivery,
	})
	if err != nil {
		s.internalError(w, "queue deploy", err)
		return
	}
	if !created {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.queue.Wake()
	writeJSON(w, http.StatusAccepted, deploy)
}

func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	project, err := s.store.ProjectByName(r.Context(), r.PathValue("project"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return store.Project{}, false
	}
	if err != nil {
		s.internalError(w, "load project", err)
		return store.Project{}, false
	}
	return project, true
}

func (s *Server) loadDeploy(w http.ResponseWriter, r *http.Request) (store.Deploy, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "deploy not found")
		return store.Deploy{}, false
	}
	deploy, err := s.store.Deploy(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deploy not found")
		return store.Deploy{}, false
	}
	if err != nil {
		s.internalError(w, "load deploy", err)
		return store.Deploy{}, false
	}
	return deploy, true
}

func (s *Server) internalError(w http.ResponseWriter, action string, err error) {
	s.log.Error(action, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// slugify turns a repo name into a DNS label.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
