package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	if err = s.indexHistoryCommit(context.Background(), p.ID, repo, second); err != nil {
		t.Fatal(err)
	}
	var checkpoint int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_history_queue WHERE project_id=$1`, p.ID).Scan(&checkpoint); err != nil || checkpoint != 1 {
		t.Fatalf("checkpoint queue=%d: %v", checkpoint, err)
	}
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
	model := &scriptedProvider{replies: []string{"安全约束：检查归属。\n待验证线索：检查其他调用者。"}}
	s.cfgMu.Lock()
	s.llmProv, s.llmOn = model, true
	s.cfgMu.Unlock()
	endpoint := "/api/projects/" + p.ID + "/history/commits/" + second + "/analyses"
	body := `{"question":"检查其他调用者是否遗漏归属校验","idempotency_key":"same-request"}`
	var queued struct {
		ID int64 `json:"id"`
	}
	for i := 0; i < 2; i++ {
		w = projectRequest(t, s, token, http.MethodPost, endpoint, "application/json", strings.NewReader(body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("enqueue: %d %s", w.Code, w.Body.String())
		}
		var response struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			queued = response
		} else if response.ID != queued.ID {
			t.Fatalf("duplicate analysis: %d != %d", response.ID, queued.ID)
		}
	}
	job, err := s.claimHistoryAnalysis()
	if err != nil || job == nil || job.ID != queued.ID {
		t.Fatalf("claim: %+v %v", job, err)
	}
	s.performHistoryAnalysis(context.Background(), model, *job)
	var state, conclusion, depth string
	var taskID sql.NullInt64
	if err := s.m.pg.QueryRow(`SELECT state,conclusion,task_id FROM project_fix_analysis WHERE id=$1`, job.ID).Scan(&state, &conclusion, &taskID); err != nil {
		t.Fatal(err)
	}
	if state != "done" || !strings.Contains(conclusion, "归属") || taskID.Valid {
		t.Fatalf("analysis: state=%s conclusion=%q task=%v", state, conclusion, taskID)
	}
	if err := s.m.pg.QueryRow(`SELECT depth FROM project_fix_candidates WHERE project_id=$1 AND primary_sha=$2`, p.ID, second).Scan(&depth); err != nil || depth != "deep" {
		t.Fatalf("candidate depth=%s: %v", depth, err)
	}
	w = projectRequest(t, s, token, http.MethodGet, endpoint, "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "检查其他调用者") {
		t.Fatalf("saved analysis: %d %s", w.Code, w.Body.String())
	}
	w = projectRequest(t, s, token, http.MethodPost, endpoint, "application/json", strings.NewReader(`{"question":"继续检查异常处理路径是否缺少校验","idempotency_key":"next-round"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("second round: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.m.pg.Exec(`UPDATE project_fix_analysis SET state='running',job_id='interrupted',lease_until=now()-interval '1 minute'
		WHERE project_id=$1 AND analysis_key='next-round'`, p.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.claimHistoryAnalysis()
	if err != nil || resumed == nil || resumed.JobID == "interrupted" {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	s.performHistoryAnalysis(context.Background(), model, *resumed)
	var rounds int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM project_fix_analysis WHERE project_id=$1 AND commit_sha=$2 AND state='done'`, p.ID, second).Scan(&rounds); err != nil || rounds != 2 {
		t.Fatalf("saved rounds=%d: %v", rounds, err)
	}
}

func TestHistorySyncIncrementalAndRewrite(t *testing.T) {
	s, _ := projectTestServer(t)
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
	write("func Handle() { authorize(user, owner); validate(input) }\n", "follow-up")
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
	config, err := os.ReadFile(filepath.Join(s.historyDir(p.ID), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), token) || strings.Contains(string(config), "extraHeader") {
		t.Fatal("credential persisted in history object store")
	}
}
