package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	valid := map[string]Repo{
		"https://github.com/o/r":      {"o", "r"},
		"https://github.com/o/r.git":  {"o", "r"},
		"https://github.com/o/r/":     {"o", "r"},
		" https://github.com/o/a.b ":  {"o", "a.b"},
		"https://github.com/O-x/R_y1": {"O-x", "R_y1"},
	}
	for raw, want := range valid {
		got, err := ParseRepoURL(raw)
		if err != nil || got != want {
			t.Errorf("ParseRepoURL(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}

	invalid := []string{
		"", "o/r", "http://github.com/o/r", "https://gitlab.com/o/r",
		"https://github.com/o", "https://github.com/o/r/tree/main", "https://github.com/o/..%2f",
	}
	for _, raw := range invalid {
		if got, err := ParseRepoURL(raw); err == nil {
			t.Errorf("ParseRepoURL(%q) = %v, want error", raw, got)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"zen":"ok"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !VerifySignature("secret", body, valid) {
		t.Error("valid signature rejected")
	}
	for name, ok := range map[string]bool{
		"wrong secret": VerifySignature("other", body, valid),
		"changed body": VerifySignature("secret", []byte("x"), valid),
		"missing":      VerifySignature("secret", body, ""),
		"sha1 prefix":  VerifySignature("secret", body, strings.Replace(valid, "sha256", "sha1", 1)),
		"not hex":      VerifySignature("secret", body, "sha256=zz"),
	} {
		if ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPushBranch(t *testing.T) {
	if got := (Push{Ref: "refs/heads/feat/x"}).Branch(); got != "feat/x" {
		t.Errorf("Branch() = %q", got)
	}
	if got := (Push{Ref: "refs/tags/v1"}).Branch(); got != "" {
		t.Errorf("Branch() = %q, want empty for a tag", got)
	}
}

func TestCreateWebhookAndStatus(t *testing.T) {
	var (
		paths []string
		auth  string
		hook  struct {
			Events []string          `json:"events"`
			Config map[string]string `json:"config"`
		}
		status map[string]string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/hooks") {
			_ = json.Unmarshal(body, &hook)
		} else {
			_ = json.Unmarshal(body, &status)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer server.Close()

	c := NewClient("tok", server.URL, "")
	repo := Repo{"o", "r"}

	id, err := c.CreateWebhook(t.Context(), repo, "https://x/hook", "s3cret")
	if err != nil || id != 7 {
		t.Fatalf("CreateWebhook = %d, %v", id, err)
	}
	if hook.Config["secret"] != "s3cret" || hook.Config["content_type"] != "json" || hook.Events[0] != "push" {
		t.Errorf("hook = %+v", hook)
	}

	if err := c.SetStatus(t.Context(), repo, "abc", StateFailure, strings.Repeat("x", 200), ""); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(status["description"])); got != 140 {
		t.Errorf("description length = %d", got)
	}
	if _, ok := status["target_url"]; ok {
		t.Error("empty target_url was sent")
	}
	if status["context"] != StatusContext {
		t.Errorf("context = %q", status["context"])
	}
	if paths[1] != "/repos/o/r/statuses/abc" || auth != "Bearer tok" {
		t.Errorf("path = %s auth = %s", paths[1], auth)
	}
}

func TestAPIErrorIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Resource not accessible"}`, http.StatusForbidden)
	}))
	defer server.Close()

	_, err := NewClient("tok", server.URL, "").CreateWebhook(context.Background(), Repo{"o", "r"}, "u", "s")
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchChecksOutOneCommit(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "o", "r.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "Dockerfile"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("-C", repoDir, "add", ".")
	git("-C", repoDir, "commit", "--quiet", "-m", "v1")
	sha := git("-C", repoDir, "rev-parse", "HEAD")

	dest := filepath.Join(t.TempDir(), "checkout")
	c := NewClient("tok", "", "file://"+base)
	if err := c.Fetch(t.Context(), Repo{"o", "r"}, sha, dest, io.Discard); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "Dockerfile"))
	if err != nil || string(got) != "v1" {
		t.Errorf("Dockerfile = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git should be removed, stat err = %v", err)
	}
}

func TestFetchRejectsNonSHA(t *testing.T) {
	err := NewClient("tok", "", "").Fetch(t.Context(), Repo{"o", "r"}, "--upload-pack=x", t.TempDir(), io.Discard)
	if err == nil {
		t.Error("expected error")
	}
}
