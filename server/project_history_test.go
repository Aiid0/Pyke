package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aiid0/Pyke/db"
	"github.com/google/uuid"
)

func TestHistoryScreeningKeepsBehavioralFix(t *testing.T) {
	messageScore, _ := historyScore("tune request handler", []string{"server/handler.go"})
	codeScore, reasons := historyDiffScore("diff --git a/server/handler.go b/server/handler.go\n+if !authorize(user, owner) { return forbidden }\n")
	if messageScore != 0 || codeScore == 0 || len(reasons) == 0 {
		t.Fatalf("hidden fix: message=%d code=%d reasons=%v", messageScore, codeScore, reasons)
	}
	if score, _ := historyScore("revert follow-up permission fix", nil); score == 0 {
		t.Fatal("follow-up lost")
	}
}

func TestHistoryCommitCheckpointAndDedup(t *testing.T) {
	s, token := projectTestServer(t)
	p, err := s.m.pg.CreateProject(db.Project{ID: "history-" + strings.ReplaceAll(t.Name(), "/", "-"), Name: "history", SourceType: "git", RepositoryURL: "https://example.invalid/repo.git", Branch: "main", FileCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM projects WHERE id=$1`, p.ID) })
	repo := s.historyDir(p.ID)
	if err = os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=History Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=History Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err = os.WriteFile(filepath.Join(repo, "handler.go"), []byte("func Handle() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "handler.go")
	git("commit", "-qm", "ordinary handler")
	first := git("rev-parse", "HEAD")
	if err = os.WriteFile(filepath.Join(repo, "handler.go"), []byte("func Handle() { authorize(user, owner) }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "handler.go")
	git("commit", "-qm", "adjust handler")
	second := git("rev-parse", "HEAD")
	if _, err = s.m.pg.Exec(`INSERT INTO project_history_queue(project_id,commit_sha) VALUES($1,$2)`, p.ID, second); err != nil {
		t.Fatal(err)
	}
	if err = s.indexHistoryCommit(context.Background(),p.ID,repo,second);err!=nil{t.Fatal(err)}
	var checkpoint int
	if err=s.m.pg.QueryRow(`SELECT count(*) FROM project_history_queue WHERE project_id=$1`,p.ID).Scan(&checkpoint);err!=nil||checkpoint!=1{t.Fatalf("checkpoint queue=%d: %v",checkpoint,err)}
	for _, sha := range []string{first, second} {
		if err = s.indexHistoryCommit(context.Background(), p.ID, repo, sha); err != nil {
			t.Fatal(err)
		}
	}
	var commits, queue, candidates int
	if err = s.m.pg.QueryRow(`SELECT count(*),count(*) FILTER(WHERE score>0) FROM project_history_commits WHERE project_id=$1`, p.ID).Scan(&commits, &candidates); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_history_queue WHERE project_id=$1`, p.ID).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if commits != 2 || queue != 0 || candidates != 1 {
		t.Fatalf("commits=%d queue=%d candidates=%d", commits, queue, candidates)
	}
	var cards int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_fix_candidates WHERE project_id=$1`, p.ID).Scan(&cards); err != nil || cards != 1 {
		t.Fatalf("candidate cards=%d: %v", cards, err)
	}
	var excerpt string
	if err = s.m.pg.QueryRow(`SELECT diff_excerpt FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2`, p.ID, second).Scan(&excerpt); err != nil || !strings.Contains(excerpt, "authorize") {
		t.Fatalf("excerpt: %v %q", err, excerpt)
	}
	w := projectRequest(t, s, token, http.MethodGet, "/api/projects/"+p.ID+"/history/commits?candidates=1", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("candidate endpoint: %d %s", w.Code, w.Body.String())
	}
	var listed struct {
		Items []historyCommit `json:"items"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed.Items) != 1 || listed.Items[0].SHA != second {
		t.Fatalf("candidate response: %v %+v", err, listed)
	}
	w = projectRequest(t, s, token, http.MethodGet, "/api/projects/"+p.ID+"/history/commits/"+second+"/diff?path=handler.go", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "authorize") {
		t.Fatalf("diff endpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestHistorySyncIncrementalAndRewrite(t *testing.T) {
	s, auth := projectTestServer(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=History Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=History Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", work)
	write := func(content, subject string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "handler.go"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		git("-C", work, "add", "handler.go")
		git("-C", work, "commit", "-qm", subject)
	}
	write("func Handle() {}\n", "initial")
	first := git("-C", work, "rev-parse", "HEAD")
	write("func Handle() { authorize(user, owner) }\n", "adjust handler")
	second := git("-C", work, "rev-parse", "HEAD")
	bare := filepath.Join(root, "repo.git")
	git("clone", "-q", "--bare", work, bare)
	backend := filepath.Join(git("--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip(err)
	}
	const token = "history-test-token"
	cgiHandler := &cgi.Handler{Path: backend, Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "tester" || p != token {
			w.WriteHeader(401)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer service.Close()
	p, err := s.m.pg.CreateProject(db.Project{ID: uuid.NewString(), Name: "history", SourceType: "git", RepositoryURL: service.URL + "/repo.git", Branch: "main", Revision: second, FileCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM projects WHERE id=$1`, p.ID) })
	sync := func() {
		t.Helper()
		if err := s.runHistorySync(context.Background(), p, "tester", token); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM project_history_commits WHERE project_id=$1`, p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sync()
	if got := count(); got != 2 {
		t.Fatalf("initial indexed=%d", got)
	}
	source := filepath.Join(s.m.dir, "projects", p.ID, "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "handler.go"), []byte("func Handle() { authorize(user, owner) }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	createTask := func() *Task {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": "history task", "description": "review fixed source", "goal": "check previous fix", "project_id": p.ID})
		w := projectRequest(t, s, auth, http.MethodPost, "/api/tasks", "application/json", strings.NewReader(string(body)))
		if w.Code != http.StatusCreated {
			t.Fatalf("create task: %d %s", w.Code, w.Body.String())
		}
		var dto TaskDTO
		if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		id, err := strconv.ParseInt(dto.ID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.m.pg.DeleteTask(id) })
		task := s.m.ResolveTask(dto.ID)
		if task == nil {
			t.Fatal("created task missing")
		}
		return task
	}
	oldTask := createTask()
	write("func Handle() { authorize(user, owner); validate(input) }\n", "follow-up")
	third := git("-C", work, "rev-parse", "HEAD")
	git("-C", work, "push", "-q", bare, "main")
	sync()
	if got := count(); got != 3 {
		t.Fatalf("incremental indexed=%d", got)
	}
	git("-C", work, "reset", "--hard", first)
	write("func Handle() { permissionCheck(user) }\n", "replacement")
	git("-C", work, "push", "-q", "--force", bare, "main")
	sync()
	if got := count(); got != 4 {
		t.Fatalf("rewritten indexed=%d", got)
	}
	var orphaned, rewritten bool
	if err := s.m.pg.QueryRow(`SELECT orphaned FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2`, p.ID, second).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`SELECT rewritten FROM project_history_refs WHERE project_id=$1 AND ref_name='main'`, p.ID).Scan(&rewritten); err != nil {
		t.Fatal(err)
	}
	if !orphaned || !rewritten {
		t.Fatalf("force push: orphaned=%t rewritten=%t", orphaned, rewritten)
	}
	oldLead, err := oldTask.Store.GetTaskFixLead(second)
	if err != nil || oldLead == nil {
		t.Fatalf("old task lost its fixed commit: %+v %v", oldLead, err)
	}
	if newer, err := oldTask.Store.GetTaskFixLead(third); err != nil || newer != nil {
		t.Fatalf("newer commit leaked into old task: %+v %v", newer, err)
	}
	if _, err := oldTask.Store.RecordTaskFixAnalysis(second, "检查同一鉴权函数的平行入口", "当前快照仅有一个入口，未见可达旁路", json.RawMessage(`{"path":"handler.go"}`)); err != nil {
		t.Fatal(err)
	}
	if err := oldTask.Store.SetTaskFixVerdict(second, "ruled_out", "当前快照仅有一个入口，未找到可达旁路", 0); err != nil {
		t.Fatal(err)
	}
	nextTask := createTask()
	nextLead, err := nextTask.Store.GetTaskFixLead(second)
	if err != nil || nextLead == nil || nextLead.VerificationState != "unverified" {
		t.Fatalf("task verdict leaked: %+v %v", nextLead, err)
	}
	shared, err := nextTask.Store.TaskFixAnalyses(second)
	if err != nil || len(shared) == 0 {
		t.Fatalf("project analysis not reusable: %v %v", shared, err)
	}
	config, err := os.ReadFile(filepath.Join(s.historyDir(p.ID), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), token) || strings.Contains(string(config), "extraHeader") {
		t.Fatal("credential persisted in history object store")
	}
}
