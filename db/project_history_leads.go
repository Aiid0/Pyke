package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// HistoryLead is a project-level source clue reachable from a task's fixed revision.
type HistoryLead struct {
	CommitSHA      string     `json:"commit_sha"`
	AuthoredAt     *time.Time `json:"authored_at,omitempty"`
	Subject        string     `json:"subject"`
	Files          []string   `json:"files"`
	Score          int        `json:"score"`
	Reasons        []string   `json:"reasons"`
	State          string     `json:"state"`
	Depth          string     `json:"depth"`
	CurrentSummary string     `json:"current_summary,omitempty"`
	DiffExcerpt    string     `json:"diff_excerpt,omitempty"`
	DiffTruncated  bool       `json:"diff_truncated,omitempty"`
}

type HistoryLeadAnalysis struct {
	Question   string    `json:"question"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
}

const reachableHistoryLeads = `WITH RECURSIVE reachable(sha) AS (
 SELECT $2::text UNION
 SELECT parent.sha FROM reachable r
 JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
 CROSS JOIN LATERAL unnest(c.parents) AS parent(sha)
) `

func (s *ExplorationStore) historyProjectRevision(ctx context.Context) (*Project, error) {
	p, err := s.TaskProject()
	if err != nil || p == nil || p.SourceType != "git" || p.Revision == "" {
		return nil, err
	}
	var indexed bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_history_commits WHERE project_id=$1 AND commit_sha=$2)`, p.ID, p.Revision).Scan(&indexed)
	if err != nil || !indexed {
		return nil, err
	}
	return p, nil
}

// SearchTaskHistoryLeads only returns commits reachable from this task's source
// revision. An absent index yields no leads, so ordinary hunting can proceed.
func (s *ExplorationStore) SearchTaskHistoryLeads(ctx context.Context, limit, offset int, query string) ([]HistoryLead, error) {
	if limit < 1 || limit > 20 {
		limit = 5
	}
	if offset < 0 || offset > 100000 {
		return nil, errors.New("无效分页位置")
	}
	query = strings.TrimSpace(query)
	if len(query) > 200 {
		return nil, errors.New("查询关键词过长")
	}
	p, err := s.historyProjectRevision(ctx)
	if err != nil || p == nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, reachableHistoryLeads+`SELECT c.commit_sha,c.authored_at,c.subject,array_to_json(c.files)::text,c.score,
		array_to_json(c.reasons)::text,k.state,k.depth,left(k.current_summary,1200)
		FROM reachable r JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
		JOIN project_fix_candidates k ON k.project_id=c.project_id AND k.primary_sha=c.commit_sha
		WHERE ($5='' OR c.subject ILIKE '%'||$5||'%' OR EXISTS (SELECT 1 FROM unnest(c.files) f WHERE f ILIKE '%'||$5||'%'))
		ORDER BY CASE WHEN k.depth='deep' THEN 1 ELSE 0 END DESC,k.score DESC,c.authored_at DESC,c.commit_sha
		LIMIT $3 OFFSET $4`, p.ID, p.Revision, limit, offset, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryLead{}
	for rows.Next() {
		var lead HistoryLead
		var at sql.NullTime
		var files, reasons string
		if err := rows.Scan(&lead.CommitSHA, &at, &lead.Subject, &files, &lead.Score, &reasons, &lead.State, &lead.Depth, &lead.CurrentSummary); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(files), &lead.Files); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reasons), &lead.Reasons); err != nil {
			return nil, err
		}
		if at.Valid {
			lead.AuthoredAt = &at.Time
		}
		out = append(out, lead)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) GetTaskHistoryLead(ctx context.Context, sha string) (*HistoryLead, []HistoryLeadAnalysis, error) {
	if !historyLeadSHA(sha) {
		return nil, nil, errors.New("提交 SHA 无效")
	}
	p, err := s.historyProjectRevision(ctx)
	if err != nil || p == nil {
		return nil, nil, err
	}
	var lead HistoryLead
	var at sql.NullTime
	var files, reasons string
	err = s.db.QueryRowContext(ctx, reachableHistoryLeads+`SELECT c.commit_sha,c.authored_at,c.subject,array_to_json(c.files)::text,c.score,
		array_to_json(c.reasons)::text,COALESCE(k.state,''),COALESCE(k.depth,''),COALESCE(left(k.current_summary,1200),''),
		left(c.diff_excerpt,16384),(c.diff_truncated OR length(c.diff_excerpt)>16384)
		FROM reachable r JOIN project_history_commits c ON c.project_id=$1 AND c.commit_sha=r.sha
		LEFT JOIN project_fix_candidates k ON k.project_id=c.project_id AND k.primary_sha=c.commit_sha
		WHERE c.commit_sha=$3`, p.ID, p.Revision, sha).
		Scan(&lead.CommitSHA, &at, &lead.Subject, &files, &lead.Score, &reasons, &lead.State, &lead.Depth, &lead.CurrentSummary, &lead.DiffExcerpt, &lead.DiffTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(files), &lead.Files); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(reasons), &lead.Reasons); err != nil {
		return nil, nil, err
	}
	if at.Valid {
		lead.AuthoredAt = &at.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT question,left(conclusion,2400),created_at FROM project_fix_analysis
		WHERE project_id=$1 AND commit_sha=$2 AND task_id IS NULL AND state='done' ORDER BY id DESC LIMIT 3`, p.ID, sha)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	analyses := []HistoryLeadAnalysis{}
	for rows.Next() {
		var a HistoryLeadAnalysis
		if err := rows.Scan(&a.Question, &a.Conclusion, &a.CreatedAt); err != nil {
			return nil, nil, err
		}
		analyses = append(analyses, a)
	}
	return &lead, analyses, rows.Err()
}

func historyLeadSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
