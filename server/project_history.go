package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aiid0/Pyke/db"
	"github.com/google/uuid"
)

const historyBatchSize = 100
const historyScreenVersion = 1

var historySHAPattern = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
var historyJobs sync.Map // one active history worker per project in this process
var historySlots = make(chan struct{}, 2)

type historyStatus struct {
	State      string `json:"state"`
	Head       string `json:"head_sha"`
	Branch     string `json:"branch"`
	Indexed    int64  `json:"indexed"`
	Candidates int64  `json:"candidates"`
	Pending    int64  `json:"pending"`
	Error      string `json:"last_error"`
	Shallow    bool   `json:"shallow"`
	Rewritten  bool   `json:"rewritten"`
}

type historyCommit struct {
	SHA            string    `json:"commit_sha"`
	Parents        []string  `json:"parents"`
	Authored       time.Time `json:"authored_at"`
	Subject        string    `json:"subject"`
	Files          []string  `json:"files"`
	Score          int       `json:"score"`
	Reasons        []string  `json:"reasons"`
	Orphaned       bool      `json:"orphaned"`
	CandidateID    int64     `json:"candidate_id,omitempty"`
	CandidateState string    `json:"candidate_state,omitempty"`
	Depth          string    `json:"depth,omitempty"`
}

func (s *Server) historyProject(w http.ResponseWriter, r *http.Request) *db.Project {
	p, err := s.m.pg.GetProject(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, "无法读取项目")
		return nil
	}
	if p == nil {
		writeErr(w, 404, "项目不存在")
		return nil
	}
	if p.SourceType != "git" {
		writeErr(w, 409, "只有 Git 项目支持历史分析")
		return nil
	}
	return p
}

func (s *Server) historyDir(projectID string) string {
	return filepath.Join(s.m.dir, "projects", projectID, "history.git")
}

// historyGit never writes credentials to the command line, Git config or log.
func historyGit(ctx context.Context, dir, username, token string, args ...string) (string, error) {
	out, truncated, err := historyGitOutput(ctx, dir, username, token, 2<<20, args...)
	if err != nil {
		return "", err
	}
	if truncated {
		return "", errors.New("Git 输出超过上限")
	}
	return strings.TrimSpace(out), nil
}

type historyOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (w *historyOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		w.data = append(w.data, p[:remaining]...)
	}
	if n > remaining {
		w.truncated = true
	}
	return n, nil
}

func historyGitOutput(ctx context.Context, dir, username, token string, limit int, args ...string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never"}, args...)...)
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	if token != "" {
		if username == "" {
			username = "oauth2"
		}
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+token)))
	}
	cmd.Env = env
	output := &historyOutput{limit: limit}
	cmd.Stdout = output
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return "", false, err
	}
	return strings.ToValidUTF8(string(output.data), "�"), output.truncated, nil
}

func (s *Server) historyStatus(w http.ResponseWriter, r *http.Request) {
	p := s.historyProject(w, r)
	if p == nil {
		return
	}
	status := historyStatus{State: "disabled", Branch: p.Branch}
	err := s.m.pg.QueryRow(`SELECT CASE WHEN state='running' AND (lease_until IS NULL OR lease_until<now()) THEN 'interrupted' ELSE state END,
		head_sha,branch,indexed,candidates,last_error FROM project_history WHERE project_id=$1`, p.ID).
		Scan(&status.State, &status.Head, &status.Branch, &status.Indexed, &status.Candidates, &status.Error)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 500, "读取历史状态失败")
		return
	}
	if status.State != "disabled" {
		if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_history_commits WHERE project_id=$1`, p.ID).Scan(&status.Indexed); err != nil {
			writeErr(w, 500, "读取历史进度失败")
			return
		}
		if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_fix_candidates WHERE project_id=$1`, p.ID).Scan(&status.Candidates); err != nil {
			writeErr(w, 500, "读取候选进度失败")
			return
		}
		if err = s.m.pg.QueryRow(`SELECT rewritten FROM project_history_refs WHERE project_id=$1 AND ref_name=$2`, p.ID, status.Branch).Scan(&status.Rewritten); err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 500, "读取分支状态失败")
			return
		}
	}
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM project_history_queue WHERE project_id=$1`, p.ID).Scan(&status.Pending); err != nil {
		writeErr(w, 500, "读取待处理进度失败")
		return
	}
	if status.State != "disabled" {
		shallow, e := historyGit(r.Context(), s.historyDir(p.ID), "", "", "rev-parse", "--is-shallow-repository")
		status.Shallow = e == nil && shallow == "true"
	}
	writeJSON(w, 200, status)
}

func (s *Server) historySync(w http.ResponseWriter, r *http.Request) {
	p := s.historyProject(w, r)
	if p == nil {
		return
	}
	var input struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, "历史同步参数无效")
			return
		}
	}
	select {
	case historySlots <- struct{}{}:
	default:
		writeErr(w, http.StatusTooManyRequests, "历史同步任务已满，请稍后重试")
		return
	}
	if _, busy := historyJobs.LoadOrStore(p.ID, true); busy {
		<-historySlots
		writeErr(w, 409, "该项目历史正在同步")
		return
	}
	jobID := uuid.NewString()
	var claimed string
	if err := s.m.pg.QueryRow(`INSERT INTO project_history(project_id,state,branch,job_id,lease_until)
		VALUES($1,'running',$2,$3,now()+interval '2 minutes')
		ON CONFLICT(project_id) DO UPDATE SET state='running',last_error='',job_id=$3,
			lease_until=now()+interval '2 minutes',updated_at=now()
		WHERE project_history.state<>'running' OR project_history.lease_until IS NULL OR project_history.lease_until<now()
		RETURNING job_id`, p.ID, p.Branch, jobID).Scan(&claimed); err != nil {
		historyJobs.Delete(p.ID)
		<-historySlots
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 409, "该项目历史正在同步")
			return
		}
		writeErr(w, 500, "无法启动历史同步")
		return
	}
	go func() {
		defer historyJobs.Delete(p.ID)
		defer func() { <-historySlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					result, err := s.m.pg.Exec(`UPDATE project_history SET lease_until=now()+interval '2 minutes'
					WHERE project_id=$1 AND job_id=$2 AND state='running'`, p.ID, jobID)
					if err != nil {
						cancel()
						return
					}
					if n, _ := result.RowsAffected(); n == 0 {
						cancel()
						return
					}
				}
			}
		}()
		err := s.runHistorySync(ctx, p, input.Username, input.Token)
		cancel()
		<-heartbeatDone
		state, message := "ready", ""
		if err != nil {
			state, message = "interrupted", err.Error()
		}
		_, _ = s.m.pg.Exec(`UPDATE project_history SET state=$2,last_error=$3,lease_until=NULL,updated_at=now(),
			indexed=(SELECT count(*) FROM project_history_commits WHERE project_id=$1),
			candidates=(SELECT count(*) FROM project_fix_candidates WHERE project_id=$1)
			WHERE project_id=$1 AND job_id=$4`, p.ID, state, message, jobID)
	}()
	writeJSON(w, 202, map[string]any{"state": "running", "project_id": p.ID, "job_id": jobID})
}

func (s *Server) runHistorySync(ctx context.Context, p *db.Project, username, token string) error {
	dir := s.historyDir(p.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if _, err = historyGit(ctx, dir, "", "", "init", "--bare", "--quiet"); err != nil {
			return errors.New("无法初始化 Git 历史库")
		}
	}
	shallow, _ := historyGit(ctx, dir, "", "", "rev-parse", "--is-shallow-repository")
	args := []string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules"}
	if shallow == "true" {
		args = append(args, "--deepen=200")
	} else if shallow != "true" {
		var count int
		_ = s.m.pg.QueryRow(`SELECT count(*) FROM project_history_commits WHERE project_id=$1`, p.ID).Scan(&count)
		if count == 0 {
			args = append(args, "--depth=200")
		}
	}
	branch := p.Branch
	if branch == "" {
		branch = "HEAD"
	}
	args = append(args, "--", p.RepositoryURL, branch)
	if _, err := historyGit(ctx, dir, username, token, args...); err != nil {
		return errors.New("Git 历史拉取失败，请检查仓库权限与网络")
	}
	head, err := historyGit(ctx, dir, "", "", "rev-parse", "FETCH_HEAD")
	if err != nil || !historySHAPattern.MatchString(head) {
		return errors.New("Git 未返回有效提交")
	}
	var oldHead string
	_ = s.m.pg.QueryRow(`SELECT head_sha FROM project_history_refs WHERE project_id=$1 AND ref_name=$2`, p.ID, branch).Scan(&oldHead)
	rewritten := false
	if oldHead != "" && oldHead != head {
		_, ancestryErr := historyGit(ctx, dir, "", "", "merge-base", "--is-ancestor", oldHead, head)
		rewritten = ancestryErr != nil
	}
	if _, err = s.m.pg.Exec(`INSERT INTO project_history_refs(project_id,ref_name,head_sha,rewritten) VALUES($1,$2,$3,$4)
		ON CONFLICT(project_id,ref_name) DO UPDATE SET previous_head_sha=project_history_refs.head_sha,
		head_sha=$3,rewritten=$4,updated_at=now()`, p.ID, branch, head, rewritten); err != nil {
		return err
	}
	if _, err = s.m.pg.Exec(`INSERT INTO project_history_queue(project_id,commit_sha) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.ID, head); err != nil {
		return err
	}
	if _, err = s.m.pg.Exec(`UPDATE project_history SET head_sha=$2,branch=$3,updated_at=now() WHERE project_id=$1`, p.ID, head, branch); err != nil {
		return err
	}
	if _, err = s.m.pg.Exec(`INSERT INTO project_history_queue(project_id,commit_sha)
		SELECT project_id,commit_sha FROM project_history_commits WHERE project_id=$1 AND screen_version<$2
		ON CONFLICT DO NOTHING`, p.ID, historyScreenVersion); err != nil {
		return err
	}
	for {
		rows, err := s.m.pg.QueryContext(ctx, `SELECT commit_sha FROM project_history_queue WHERE project_id=$1 ORDER BY commit_sha LIMIT $2`, p.ID, historyBatchSize)
		if err != nil {
			return err
		}
		batch := []string{}
		for rows.Next() {
			var sha string
			if err = rows.Scan(&sha); err != nil {
				break
			}
			batch = append(batch, sha)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			if rewritten {
				_, err = s.m.pg.ExecContext(ctx, historyReachableSQL+`UPDATE project_history_commits c SET orphaned=NOT EXISTS(
					SELECT 1 FROM reachable r WHERE r.sha=c.commit_sha) WHERE c.project_id=$1`, p.ID, head)
				if err != nil {
					return err
				}
			}
			return nil
		}
		missing := false
		for _, sha := range batch {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, e := historyGit(ctx, dir, "", "", "cat-file", "-e", sha+"^{commit}"); e != nil {
				missing = true
				continue
			}
			if err := s.indexHistoryCommit(ctx, p.ID, dir, sha); err != nil {
				return err
			}
		}
		if missing {
			return errors.New("历史尚未拉取到更早提交；再次同步可继续加深")
		}
	}
}

func (s *Server) indexHistoryCommit(ctx context.Context, projectID, dir, sha string) error {
	meta, err := historyGit(ctx, dir, "", "", "show", "-s", "--format=%P%x00%aI%x00%s", sha)
	if err != nil {
		return errors.New("无法读取提交元数据")
	}
	parts := strings.SplitN(meta, "\x00", 3)
	if len(parts) != 3 {
		return errors.New("提交元数据损坏")
	}
	parents := strings.Fields(parts[0])
	authored, _ := time.Parse(time.RFC3339, parts[1])
	filesRaw, filesTruncated, err := historyGitOutput(ctx, dir, "", "", 2<<20, "diff-tree", "--root", "-m", "--first-parent", "--no-commit-id", "--name-only", "-z", "-r", sha)
	if err != nil {
		return errors.New("无法读取提交文件列表")
	}
	files := []string{}
	fileNames := strings.Split(filesRaw, "\x00")
	if filesTruncated && !strings.HasSuffix(filesRaw, "\x00") {
		fileNames = fileNames[:len(fileNames)-1]
	}
	for _, file := range fileNames {
		if file != "" && len(files) < 200 {
			files = append(files, file)
		}
	}
	score, reasons := historyScore(parts[2], files)
	if filesTruncated || len(fileNames) > 201 {
		score++
		reasons = append(reasons, "改动文件过多，需按需展开")
	}
	diffExcerpt, diffTruncated := "", false
	if len(files) <= 50 {
		if diff, oversized, e := historyGitOutput(ctx, dir, "", "", 2<<20, "show", "--format=", "--diff-merges=first-parent", "--unified=0", "--no-ext-diff", sha); e == nil {
			if extra, labels := historyDiffScore(diff); extra > 0 {
				score += extra
				reasons = append(reasons, labels...)
			}
			if oversized {
				score++
				reasons = append(reasons, "差异超过自动筛选上限，需按文件展开")
			}
			if score > 0 {
				diffTruncated = oversized || len(diff) > 65536
				if diffTruncated {
					diff = diff[:65536]
				}
				diffExcerpt = strings.ToValidUTF8(diff, "�")
			}
		}
	} else {
		score++
		reasons = append(reasons, "大提交需按文件深入筛查")
	}
	if score == 0 {
		reasons = []string{"未命中当前安全筛选规则"}
	}
	tx, err := s.m.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_history_commits(project_id,commit_sha,parents,authored_at,subject,files,score,reasons,diff_excerpt,diff_truncated,screen_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT(project_id,commit_sha) DO UPDATE SET score=EXCLUDED.score,reasons=EXCLUDED.reasons,
			diff_excerpt=EXCLUDED.diff_excerpt,diff_truncated=EXCLUDED.diff_truncated,screen_version=EXCLUDED.screen_version
		WHERE project_history_commits.screen_version<EXCLUDED.screen_version`, projectID, sha, parents, authored, parts[2], files, score, reasons, diffExcerpt, diffTruncated, historyScreenVersion); err != nil {
		return err
	}
	if score > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_fix_candidates(project_id,primary_sha,score,reasons,screen_version)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT(project_id,primary_sha) DO UPDATE SET score=EXCLUDED.score,reasons=EXCLUDED.reasons,
			state='needs_review',screen_version=EXCLUDED.screen_version,updated_at=now()
			WHERE project_fix_candidates.screen_version<EXCLUDED.screen_version`, projectID, sha, score, reasons, historyScreenVersion); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE project_fix_candidates SET state='needs_review',screen_version=$3,updated_at=now()
			WHERE project_id=$1 AND primary_sha=$2 AND screen_version<$3`, projectID, sha, historyScreenVersion); err != nil {
			return err
		}
	}
	for _, parent := range parents {
		if !historySHAPattern.MatchString(parent) {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_history_queue(project_id,commit_sha)
			SELECT $1,$2 WHERE NOT EXISTS(SELECT 1 FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2)
			ON CONFLICT DO NOTHING`, projectID, parent); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM project_history_queue WHERE project_id=$1 AND commit_sha=$2`, projectID, sha); err != nil {
		return err
	}
	return tx.Commit()
}

func historyScore(subject string, files []string) (int, []string) {
	text := strings.ToLower(subject)
	score, reasons := 0, []string{}
	for _, rule := range []struct {
		words []string
		score int
		label string
	}{
		{[]string{"security", "cve-", "ghsa-", "漏洞", "越权", "安全修复"}, 5, "提交说明涉及安全"},
		{[]string{"auth", "permission", "access control", "鉴权", "权限", "tenant"}, 3, "身份或权限边界"},
		{[]string{"sanitize", "validate", "escape", "traversal", "ssrf", "校验", "过滤", "边界"}, 2, "输入或路径校验"},
		{[]string{"revert", "follow-up", "regression", "补丁", "回退", "二次修复"}, 2, "回退或后续修复"},
	} {
		for _, word := range rule.words {
			if strings.Contains(text, word) {
				score += rule.score
				reasons = append(reasons, rule.label)
				break
			}
		}
	}
	for _, file := range files {
		f := strings.ToLower(file)
		if strings.Contains(f, "auth") || strings.Contains(f, "security") || strings.Contains(f, "permission") {
			score++
			reasons = append(reasons, "改动了安全相关路径")
			break
		}
	}
	return score, reasons
}

// Inspect only added lines: ordinary commit messages may hide a security fix.
func historyDiffScore(diff string) (int, []string) {
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		lower := strings.ToLower(line)
		for _, marker := range []string{"authorize", "permission", "checkaccess", "check_permission", "validate", "sanitize", "escape", "hmac", "constanttime", "for update", "鉴权", "权限校验", "越权"} {
			if strings.Contains(lower, marker) {
				return 2, []string{"新增安全边界相关代码"}
			}
		}
	}
	return 0, nil
}

const historyReachableSQL = `WITH RECURSIVE reachable(sha) AS (
	SELECT $2::text UNION
	SELECT parent.sha FROM reachable r JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
	CROSS JOIN LATERAL unnest(c.parents) AS parent(sha)
) `

func (s *Server) historyCommits(w http.ResponseWriter, r *http.Request) {
	p := s.historyProject(w, r)
	if p == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 30
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 || offset > 1000000 {
		writeErr(w, 400, "分页参数无效")
		return
	}
	candidateOnly := 0
	if r.URL.Query().Get("candidates") == "1" {
		candidateOnly = 1
	}
	sha := r.URL.Query().Get("sha")
	if sha != "" && !historySHAPattern.MatchString(sha) {
		writeErr(w, 400, "提交编号无效")
		return
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for _, day := range []string{from, to} {
		if day != "" {
			if _, err := time.Parse("2006-01-02", day); err != nil {
				writeErr(w, 400, "日期范围无效")
				return
			}
		}
	}
	var fromDate, toDate any
	if from != "" {
		fromDate = from
	}
	if to != "" {
		toDate = to
	}
	rows, err := s.m.pg.Query(`SELECT c.commit_sha,array_to_json(c.parents)::text,c.authored_at,c.subject,array_to_json(c.files)::text,c.score,array_to_json(c.reasons)::text,c.orphaned,k.id,k.state,k.depth
			FROM project_history_commits c LEFT JOIN project_fix_candidates k ON k.project_id=c.project_id AND k.primary_sha=c.commit_sha
			WHERE c.project_id=$1 AND ($2=0 OR k.id IS NOT NULL) AND ($5='' OR c.commit_sha=$5)
			AND ($6::date IS NULL OR c.authored_at >= $6::date) AND ($7::date IS NULL OR c.authored_at < $7::date+interval '1 day')
			ORDER BY c.score DESC,c.authored_at DESC,c.commit_sha LIMIT $3 OFFSET $4`, p.ID, candidateOnly, limit, offset, sha, fromDate, toDate)
	if err != nil {
		writeErr(w, 500, "无法读取提交")
		return
	}
	defer rows.Close()
	items := []historyCommit{}
	for rows.Next() {
		var c historyCommit
		var at sql.NullTime
		var candidateID sql.NullInt64
		var state, depth sql.NullString
		var parentsJSON, filesJSON, reasonsJSON string
		if err = rows.Scan(&c.SHA, &parentsJSON, &at, &c.Subject, &filesJSON, &c.Score, &reasonsJSON, &c.Orphaned, &candidateID, &state, &depth); err != nil {
			writeErr(w, 500, "提交数据损坏")
			return
		}
		if json.Unmarshal([]byte(parentsJSON), &c.Parents) != nil || json.Unmarshal([]byte(filesJSON), &c.Files) != nil || json.Unmarshal([]byte(reasonsJSON), &c.Reasons) != nil {
			writeErr(w, 500, "提交数组数据损坏")
			return
		}
		if candidateID.Valid {
			c.CandidateID = candidateID.Int64
			c.CandidateState = state.String
			c.Depth = depth.String
		}
		if at.Valid {
			c.Authored = at.Time
		}
		items = append(items, c)
	}
	if rows.Err() != nil {
		writeErr(w, 500, "读取提交失败")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset, "has_more": len(items) == limit})
}

func (s *Server) historyDiff(w http.ResponseWriter, r *http.Request) {
	p := s.historyProject(w, r)
	if p == nil {
		return
	}
	sha := r.PathValue("sha")
	if !historySHAPattern.MatchString(sha) {
		writeErr(w, 400, "提交编号无效")
		return
	}
	var files []string
	var filesJSON string
	if err := s.m.pg.QueryRow(`SELECT array_to_json(files)::text FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2`, p.ID, sha).Scan(&filesJSON); err != nil {
		writeErr(w, 404, "提交未索引")
		return
	}
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		writeErr(w, 500, "提交文件列表损坏")
		return
	}
	path := r.URL.Query().Get("path")
	if path != "" {
		valid := false
		for _, file := range files {
			if file == path {
				valid = true
				break
			}
		}
		if !valid {
			writeErr(w, 400, "文件不属于该提交")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	args := []string{"show", "--format=", "--diff-merges=first-parent", "--no-ext-diff", "--no-renames", "--unified=3", sha}
	if path != "" {
		args = append(args, "--", path)
	}
	diff, truncated, err := historyGitOutput(ctx, s.historyDir(p.ID), "", "", 65536, args...)
	if err != nil {
		writeErr(w, 500, "无法读取差异")
		return
	}
	writeJSON(w, 200, map[string]any{"commit_sha": sha, "path": path, "diff": diff, "truncated": truncated})
}

func (s *Server) historyAnalyses(w http.ResponseWriter, r *http.Request) {
	p := s.historyProject(w, r)
	if p == nil {
		return
	}
	sha := r.PathValue("sha")
	if !historySHAPattern.MatchString(sha) {
		writeErr(w, 400, "提交编号无效")
		return
	}
	rows, err := s.m.pg.Query(`SELECT id,question,conclusion,evidence,state,last_error,created_at FROM project_fix_analysis
		WHERE project_id=$1 AND commit_sha=$2 AND task_id IS NULL ORDER BY id DESC LIMIT 100`, p.ID, sha)
	if err != nil {
		writeErr(w, 500, "无法读取分析")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var question, conclusion, state, lastError string
		var evidence json.RawMessage
		var at time.Time
		if err := rows.Scan(&id, &question, &conclusion, &evidence, &state, &lastError, &at); err != nil {
			writeErr(w, 500, "分析数据损坏")
			return
		}
		item := map[string]any{"id": id, "question": question, "conclusion": conclusion, "evidence": evidence, "state": state, "last_error": lastError, "created_at": at}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
