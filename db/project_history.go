package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// FixLead is a project-level source lead. Its applicability is checked against
// the immutable revision bound to the calling exploration.
type FixLead struct {
	CommitSHA         string     `json:"commit_sha"`
	AuthoredAt        *time.Time `json:"authored_at,omitempty"`
	Subject           string     `json:"subject"`
	Files             []string   `json:"files"`
	Score             int        `json:"score"`
	Reasons           []string   `json:"reasons"`
	DiffExcerpt       string     `json:"diff_excerpt,omitempty"`
	DiffTruncated     bool       `json:"diff_truncated,omitempty"`
	CandidateID       int64      `json:"candidate_id,omitempty"`
	CandidateState    string     `json:"candidate_state,omitempty"`
	Depth             string     `json:"depth,omitempty"`
	VerificationState string     `json:"verification_state,omitempty"`
	VerdictNote       string     `json:"verdict_note,omitempty"`
}

func (s *ExplorationStore) historyBinding() (*Project, error) {
	p, err := s.TaskProject()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("当前任务没有源码项目")
	}
	if p.SourceType != "git" || p.Revision == "" {
		return nil, errors.New("当前任务没有可用的 Git 版本")
	}
	var indexed bool
	err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2)`, p.ID, p.Revision).Scan(&indexed)
	if err != nil {
		return nil, err
	}
	if !indexed {
		return nil, errors.New("任务绑定的源码提交尚未进入历史索引；请先同步该项目历史")
	}
	return p, nil
}

const reachableFixLeads = `WITH RECURSIVE reachable(sha) AS (
 SELECT $2::text
 UNION
 SELECT parent.sha FROM reachable r
 JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
 CROSS JOIN LATERAL unnest(c.parents) AS parent(sha)
) `

// SearchTaskFixLeads returns only commits reachable from the task's fixed source
// revision. Every request resolves the project snapshot from the task, not the
// caller's project ID, so a tool cannot read another project's history.
func (s *ExplorationStore) SearchTaskFixLeads(limit, offset int, query string) ([]FixLead, error) {
	p, err := s.historyBinding()
	if err != nil || p == nil {
		return nil, err
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	if offset < 0 || offset > 100000 {
		return nil, errors.New("无效分页位置")
	}
	query = strings.TrimSpace(query)
	if len(query) > 200 {
		return nil, errors.New("查询关键词过长")
	}
	rows, err := s.db.Query(reachableFixLeads+`SELECT c.commit_sha,c.authored_at,c.subject,array_to_json(c.files)::text,c.score,array_to_json(c.reasons)::text,k.id,k.state,k.depth,
	COALESCE(l.verification_state,'unverified'),COALESCE(l.verdict_note,'')
 FROM reachable r JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
 JOIN project_fix_candidates k ON k.project_id=c.project_id AND k.primary_sha=c.commit_sha
	LEFT JOIN task_fix_leads l ON l.candidate_id=k.id AND l.target_revision=$2 AND l.task_id=(SELECT id FROM tasks WHERE exploration_id=$6 AND deleted_at IS NULL)
 WHERE ($5='' OR c.subject ILIKE '%'||$5||'%' OR EXISTS (SELECT 1 FROM unnest(c.files) f WHERE f ILIKE '%'||$5||'%'))
 ORDER BY k.score DESC,c.authored_at DESC,c.commit_sha LIMIT $3 OFFSET $4`, p.ID, p.Revision, limit, offset, query, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FixLead{}
	for rows.Next() {
		var item FixLead
		var at sql.NullTime
		var filesJSON, reasonsJSON string
		if err := rows.Scan(&item.CommitSHA, &at, &item.Subject, &filesJSON, &item.Score, &reasonsJSON, &item.CandidateID, &item.CandidateState, &item.Depth, &item.VerificationState, &item.VerdictNote); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(filesJSON), &item.Files); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(reasonsJSON), &item.Reasons); err != nil {
			return nil, err
		}
		if at.Valid {
			item.AuthoredAt = &at.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) GetTaskFixLead(sha string) (*FixLead, error) {
	p, err := s.historyBinding()
	if err != nil || p == nil {
		return nil, err
	}
	var item FixLead
	var at sql.NullTime
	var candidateID sql.NullInt64
	var state, depth sql.NullString
	var filesJSON, reasonsJSON string
	err = s.db.QueryRow(reachableFixLeads+`SELECT c.commit_sha,c.authored_at,c.subject,array_to_json(c.files)::text,c.score,array_to_json(c.reasons)::text,c.diff_excerpt,c.diff_truncated,k.id,k.state,k.depth,
	COALESCE(l.verification_state,'unverified'),COALESCE(l.verdict_note,'')
 FROM reachable r JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
	LEFT JOIN project_fix_candidates k ON k.project_id=c.project_id AND k.primary_sha=c.commit_sha
	LEFT JOIN task_fix_leads l ON l.candidate_id=k.id AND l.target_revision=$2 AND l.task_id=(SELECT id FROM tasks WHERE exploration_id=$4 AND deleted_at IS NULL)
	WHERE c.commit_sha=$3`, p.ID, p.Revision, sha, s.expID).
		Scan(&item.CommitSHA, &at, &item.Subject, &filesJSON, &item.Score, &reasonsJSON, &item.DiffExcerpt, &item.DiffTruncated, &candidateID, &state, &depth, &item.VerificationState, &item.VerdictNote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(filesJSON), &item.Files); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(reasonsJSON), &item.Reasons); err != nil {
		return nil, err
	}
	if at.Valid {
		item.AuthoredAt = &at.Time
	}
	if candidateID.Valid {
		item.CandidateID = candidateID.Int64
		item.CandidateState = state.String
		item.Depth = depth.String
	}
	return &item, nil
}

type FixAnalysis struct {
	ID             int64           `json:"id"`
	TargetRevision string          `json:"target_revision"`
	Question       string          `json:"question"`
	Conclusion     string          `json:"conclusion"`
	Evidence       json.RawMessage `json:"evidence"`
	State          string          `json:"state"`
	CreatedAt      time.Time       `json:"created_at"`
}

func (s *ExplorationStore) TaskFixAnalyses(sha string) ([]FixAnalysis, error) {
	p, err := s.historyBinding()
	if err != nil || p == nil {
		return nil, err
	}
	lead, err := s.GetTaskFixLead(sha)
	if err != nil || lead == nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,target_revision,question,conclusion,evidence,state,created_at FROM project_fix_analysis
	 WHERE project_id=$1 AND commit_sha=$2 ORDER BY id DESC LIMIT 30`, p.ID, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FixAnalysis{}
	for rows.Next() {
		var a FixAnalysis
		if err := rows.Scan(&a.ID, &a.TargetRevision, &a.Question, &a.Conclusion, &a.Evidence, &a.State, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordTaskFixAnalysis appends a new round. It never changes a previous
// conclusion or reports a vulnerability; the task graph remains the authority
// for verified findings and evidence.
func (s *ExplorationStore) RecordTaskFixAnalysis(sha, question, conclusion string, evidence json.RawMessage) (int64, error) {
	p, err := s.historyBinding()
	if err != nil || p == nil {
		return 0, err
	}
	lead, err := s.GetTaskFixLead(sha)
	if err != nil {
		return 0, err
	}
	if lead == nil {
		return 0, errors.New("提交不属于任务目标版本")
	}
	question = strings.TrimSpace(question)
	conclusion = strings.TrimSpace(conclusion)
	if len(question) < 5 || len(question) > 2000 || len(conclusion) > 12000 {
		return 0, errors.New("分析内容长度无效")
	}
	if len(evidence) == 0 {
		evidence = []byte(`{}`)
	}
	if len(evidence) > 16384 || !json.Valid(evidence) {
		return 0, errors.New("证据 JSON 无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO project_fix_candidates(project_id,primary_sha,score,reasons)
		SELECT project_id,commit_sha,score,CASE WHEN score=0 THEN ARRAY['人工指定提交']::text[] ELSE reasons END
		FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2
		ON CONFLICT(project_id,primary_sha) DO NOTHING`, p.ID, sha); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO task_fix_leads(task_id,candidate_id,target_revision)
		SELECT t.id,c.id,$3 FROM tasks t JOIN project_fix_candidates c ON c.project_id=$1 AND c.primary_sha=$2
		WHERE t.exploration_id=$4 AND t.deleted_at IS NULL
		ON CONFLICT(task_id,candidate_id,target_revision) DO NOTHING`, p.ID, sha, p.Revision, s.expID); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO project_fix_analysis(project_id,commit_sha,task_id,target_revision,question,conclusion,evidence,state)
	 VALUES($1,$2,(SELECT id FROM tasks WHERE exploration_id=$3 AND deleted_at IS NULL),$7,$4,$5,$6,'analyzed') RETURNING id`, p.ID, sha, s.expID, question, conclusion, evidence, p.Revision).Scan(&id)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE project_fix_candidates SET state='analyzed',depth='deep',current_summary=$3,updated_at=now()
		WHERE project_id=$1 AND primary_sha=$2`, p.ID, sha, conclusion); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE project_fix_analysis SET state='analyzed',conclusion=$5,updated_at=now()
		WHERE project_id=$1 AND commit_sha=$2 AND task_id=(SELECT id FROM tasks WHERE exploration_id=$3 AND deleted_at IS NULL)
		AND question=$4 AND state='pending'`, p.ID, sha, s.expID, question, conclusion); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// SetTaskFixVerdict stores a task-specific disposition. A reported lead must
// point to a real finding from this task; source analysis alone cannot mark it.
func (s *ExplorationStore) SetTaskFixVerdict(sha, state, note string, findingID int64) error {
	p, err := s.historyBinding()
	if err != nil || p == nil {
		return err
	}
	lead, err := s.GetTaskFixLead(sha)
	if err != nil {
		return err
	}
	if lead == nil || lead.CandidateID == 0 {
		return errors.New("候选卡片不存在或不适用于任务")
	}
	note = strings.TrimSpace(note)
	if (state != "unverified" && state != "ruled_out" && state != "reported") || len(note) < 10 || len(note) > 4000 {
		return errors.New("任务结论或依据无效")
	}
	var finding any
	if state == "reported" {
		if findingID <= 0 {
			return errors.New("上报状态需要本任务的漏洞编号")
		}
		var owned bool
		err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM findings f JOIN tasks t ON t.id=f.task_id WHERE f.id=$1 AND t.exploration_id=$2 AND t.deleted_at IS NULL)`, findingID, s.expID).Scan(&owned)
		if err != nil {
			return err
		}
		if !owned {
			return errors.New("漏洞编号不属于本任务")
		}
		finding = findingID
	}
	_, err = s.db.Exec(`INSERT INTO task_fix_leads(task_id,candidate_id,target_revision,verification_state,verdict_note,finding_id)
		SELECT id,$2,$3,$4,$5,$6 FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL
		ON CONFLICT(task_id,candidate_id,target_revision) DO UPDATE SET verification_state=$4,verdict_note=$5,finding_id=$6,updated_at=now()`, s.expID, lead.CandidateID, p.Revision, state, note, finding)
	return err
}
