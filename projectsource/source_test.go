package projectsource

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestZIPPreservesProjectAndTaskCopyIsIndependent(t *testing.T) {
	raw := archive(t, map[string]string{"demo/main.go": "package main", "demo/lib/helper.go": "package lib", "demo/.git/config": "metadata"})
	root := t.TempDir()
	stats, err := ExtractZIP(bytes.NewReader(raw), int64(len(raw)), root)
	if err != nil || stats.Files != 2 {
		t.Fatalf("import: %+v %v", stats, err)
	}
	copyDir := t.TempDir()
	if _, err := Copy(root, copyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "demo", "lib", "helper.go"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(copyDir, "demo", "lib", "helper.go"))
	if err != nil || string(content) != "package lib" {
		t.Fatalf("snapshot changed: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(copyDir, "demo", ".git")); !os.IsNotExist(err) {
		t.Fatal("Git metadata copied into task snapshot")
	}
}

func TestZIPRejectsTraversalAndSymlinks(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "repo/../../escape", "C:/escape", "repo\\..\\escape"} {
		t.Run(name, func(t *testing.T) {
			raw := archive(t, map[string]string{name: "bad"})
			if _, err := ExtractZIP(bytes.NewReader(raw), int64(len(raw)), t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	var raw bytes.Buffer
	w := zip.NewWriter(&raw)
	h := &zip.FileHeader{Name: "link"}
	h.SetMode(os.ModeSymlink | 0o777)
	f, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "../outside")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractZIP(bytes.NewReader(raw.Bytes()), int64(raw.Len()), t.TempDir()); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestUploadAndCopyRejectOversizeDuplicatesAndLinks(t *testing.T) {
	root := t.TempDir()
	stats := Stats{Bytes: MaxBytes - 1}
	if err := stats.WriteFile(root, "large", strings.NewReader("xx")); err == nil {
		t.Fatal("expanded size limit ignored")
	}
	stats = Stats{}
	if err := stats.WriteFile(root, "same", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if err := stats.WriteFile(root, "same", strings.NewReader("second")); err == nil {
		t.Fatal("duplicate overwrote a project file")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := Copy(root, t.TempDir()); err == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestRepositoryURLRejectsCredentialsAndLocalProtocols(t *testing.T) {
	for _, raw := range []string{"file:///tmp/repo", "git@host:repo", "https://user:secret@host/repo", "-c evil", "https://host/repo?token=value"} {
		if _, err := RepositoryURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://gitee.com/org/repo.git", "https://github.com/org/repo", "http://git.internal/team/repo"} {
		if _, err := RepositoryURL(raw); err != nil {
			t.Error(err)
		}
	}
}

func TestCloneUsesSelectedBranchAndEphemeralAuthentication(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "work")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("main version"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", source, "add", ".")
	run("-C", source, "commit", "-m", "main")
	run("-C", source, "checkout", "-b", "audit")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("audit version"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", source, "commit", "-am", "audit")
	commit := run("-C", source, "rev-parse", "HEAD")
	run("clone", "--bare", source, filepath.Join(root, "repo.git"))
	run("--git-dir", filepath.Join(root, "repo.git"), "symbolic-ref", "HEAD", "refs/heads/main")
	backend := filepath.Join(run("--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip(err)
	}
	const testToken = "project-import-test-token"
	cgiHandler := &cgi.Handler{Path: backend, Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, token, ok := r.BasicAuth()
		if !ok || user != "test-user" || token != testToken {
			w.WriteHeader(401)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer service.Close()
	dest := filepath.Join(t.TempDir(), "clone")
	branch, revision, err := Clone(context.Background(), dest, service.URL+"/repo.git", "audit", "test-user", testToken)
	if err != nil || branch != "audit" || revision != commit {
		t.Fatalf("clone: branch=%s revision=%s err=%v", branch, revision, err)
	}
	content, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil || string(content) != "audit version" {
		t.Fatalf("wrong branch contents: %s %v", content, err)
	}
	config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), testToken) || strings.Contains(string(config), "extraHeader") {
		t.Fatal("authentication persisted in clone")
	}
}
