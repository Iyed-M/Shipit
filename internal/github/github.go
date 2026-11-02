// Package github talks to GitHub: repo URLs, webhooks, commit statuses and clones.
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// StatusContext labels Shipit's commit statuses on GitHub.
const StatusContext = "shipit"

// Commit status states accepted by GitHub.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
)

var (
	repoPart  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	shaFormat = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Repo identifies a GitHub repository.
type Repo struct {
	Owner string
	Name  string
}

// ParseRepoURL extracts the owner and name from an https://github.com URL.
func ParseRepoURL(raw string) (Repo, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return Repo{}, fmt.Errorf("%q is not an https://github.com repo URL", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return Repo{}, fmt.Errorf("%q is not an https://github.com repo URL", raw)
	}
	repo := Repo{Owner: parts[0], Name: strings.TrimSuffix(parts[1], ".git")}
	for _, part := range []string{repo.Owner, repo.Name} {
		if !repoPart.MatchString(part) || part == "." || part == ".." {
			return Repo{}, fmt.Errorf("%q is not an https://github.com repo URL", raw)
		}
	}
	return repo, nil
}

// VerifySignature checks an X-Hub-Signature-256 header against the request body.
func VerifySignature(secret string, body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Push is the part of a push webhook payload that Shipit uses.
type Push struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	HeadCommit struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

// Branch returns the pushed branch name, or "" for tags and other refs.
func (p Push) Branch() string {
	branch, _ := strings.CutPrefix(p.Ref, "refs/heads/")
	if branch == p.Ref {
		return ""
	}
	return branch
}

// Client calls the GitHub API and clones repos with one fine-grained token.
type Client struct {
	token  string
	apiURL string
	webURL string
	http   *http.Client
}

// NewClient returns a Client for the given API and web base URLs.
func NewClient(token, apiURL, webURL string) *Client {
	return &Client{
		token:  token,
		apiURL: apiURL,
		webURL: webURL,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateWebhook subscribes target to the repo's push events and returns the webhook ID.
func (c *Client) CreateWebhook(ctx context.Context, repo Repo, target, secret string) (int64, error) {
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{
			"url":          target,
			"content_type": "json",
			"secret":       secret,
		},
	}
	var hook struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/hooks", repo.Owner, repo.Name)
	if err := c.post(ctx, path, body, &hook); err != nil {
		return 0, fmt.Errorf("create webhook: %w", err)
	}
	return hook.ID, nil
}

// SetStatus posts a commit status. targetURL may be empty.
func (c *Client) SetStatus(ctx context.Context, repo Repo, sha, state, description, targetURL string) error {
	body := map[string]string{
		"state":       state,
		"context":     StatusContext,
		"description": truncate(description, 140),
	}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", repo.Owner, repo.Name, sha)
	if err := c.post(ctx, path, body, nil); err != nil {
		return fmt.Errorf("set commit status: %w", err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("GitHub answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Fetch checks out one commit of repo into dir, without its git history.
func (c *Client) Fetch(ctx context.Context, repo Repo, sha, dir string, log io.Writer) error {
	if !shaFormat.MatchString(sha) {
		return fmt.Errorf("invalid commit SHA %q", sha)
	}
	remote := fmt.Sprintf("%s/%s/%s.git", c.webURL, repo.Owner, repo.Name)

	// The token goes through the environment so it never appears in process
	// arguments, is scoped to the GitHub host, and isn't written to .git/config.
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + c.token))
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+c.webURL+"/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
	)

	for _, args := range [][]string{
		{"init", "--quiet", dir},
		{"-C", dir, "fetch", "--quiet", "--depth", "1", remote, sha},
		{"-C", dir, "checkout", "--quiet", "FETCH_HEAD"},
	} {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		cmd.Stdout = log
		cmd.Stderr = io.MultiWriter(log, &stderr)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", gitVerb(args), err, bytes.TrimSpace(stderr.Bytes()))
		}
	}
	// The Docker build context doesn't need the repo metadata.
	return os.RemoveAll(filepath.Join(dir, ".git"))
}

func gitVerb(args []string) string {
	if args[0] == "-C" {
		return args[2]
	}
	return args[0]
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

// ErrInvalidPayload is returned when a webhook body can't be decoded.
var ErrInvalidPayload = errors.New("invalid webhook payload")

// ParsePush decodes a push webhook body.
func ParsePush(body []byte) (Push, error) {
	var p Push
	if err := json.Unmarshal(body, &p); err != nil {
		return Push{}, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	return p, nil
}
