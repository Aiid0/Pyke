package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aiid0/Pyke/db"
)

func TestProjectGitMigrationResumesPendingMove(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Migration Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Migration Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", work, "add", "file.txt")
	git("-C", work, "commit", "-qm", "snapshot")
	revision := git("-C", work, "rev-parse", "HEAD")
	p := &db.Project{ID: "migration", SourceType: "git", Revision: revision}
	source := projectSourceDir(root, p.ID)
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "projects", p.ID, "history.git")
	git("clone", "-q", "--bare", work, legacy)
	pending := filepath.Join(root, "projects", p.ID, ".git.pending")
	if err := os.Rename(legacy, pending); err != nil {
		t.Fatal(err)
	}
	git("-C", pending, "update-ref", "refs/heads/pyke-snapshot", revision)
	git("-C", pending, "symbolic-ref", "HEAD", "refs/heads/pyke-snapshot")
	git("-C", pending, "config", "core.worktree", "..")
	git("-C", pending, "config", "core.bare", "false")
	s := &Server{m: &Manager{dir: root}}
	for i := 0; i < 2; i++ {
		if err := s.ensureProjectGit(context.Background(), p, "", ""); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	if got := git("-C", source, "rev-parse", "HEAD"); got != revision {
		t.Fatalf("migrated head=%s", got)
	}
	if got := git("-C", source, "status", "--porcelain"); got != "" {
		t.Fatalf("migrated source changed: %s", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy directory remains: %v", err)
	}
}
