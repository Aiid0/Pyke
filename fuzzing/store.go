package fuzzing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Aiid0/Pyke/db"
)

type Store struct{ DB *db.DB }

func (s Store) Ensure() error {
	if err := s.DB.EnsureTaskArtifactsTable(); err != nil {
		return err
	}
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS fuzz_jobs (
 id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 exploration_id BIGINT NOT NULL, creator TEXT NOT NULL, request_id TEXT NOT NULL,
 config JSONB NOT NULL, state TEXT NOT NULL DEFAULT 'queued', reason TEXT NOT NULL DEFAULT '',
 image TEXT NOT NULL DEFAULT '', snapshot TEXT NOT NULL DEFAULT '', metrics JSONB NOT NULL DEFAULT '{}',
 cursor BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_analysis TIMESTAMPTZ, auto_count INT NOT NULL DEFAULT 0, UNIQUE(task_id,request_id));
 CREATE INDEX IF NOT EXISTS fuzz_jobs_state ON fuzz_jobs(state);
 CREATE TABLE IF NOT EXISTS fuzz_groups (
 id BIGSERIAL PRIMARY KEY, job_id BIGINT NOT NULL REFERENCES fuzz_jobs(id) ON DELETE CASCADE,
 signature TEXT NOT NULL, kind TEXT NOT NULL, sample TEXT NOT NULL, detail TEXT NOT NULL,
 count INT NOT NULL DEFAULT 1, intent_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
 requested BOOLEAN NOT NULL DEFAULT false, UNIQUE(job_id,signature));
 CREATE TABLE IF NOT EXISTS fuzz_samples (
 job_id BIGINT NOT NULL REFERENCES fuzz_jobs(id) ON DELETE CASCADE,
 sample TEXT NOT NULL, group_id BIGINT NOT NULL REFERENCES fuzz_groups(id) ON DELETE CASCADE,
 PRIMARY KEY(job_id,sample));`)
	return err
}

const jobColumns = `id,task_id::text,exploration_id,creator,state,reason,config,image,snapshot,metrics,cursor,created_at`

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (Job, error) {
	var j Job
	var cfg []byte
	err := r.Scan(&j.ID, &j.TaskID, &j.ExplorationID, &j.Creator, &j.State, &j.Reason, &cfg, &j.Image, &j.Snapshot, &j.Metrics, &j.Cursor, &j.CreatedAt)
	if err == nil {
		err = json.Unmarshal(cfg, &j.Config)
	}
	return j, err
}
func (s Store) Create(task string, exp int64, creator string, c Config) (Job, error) {
	b, _ := json.Marshal(c)
	return scanJob(s.DB.QueryRow(`INSERT INTO fuzz_jobs(task_id,exploration_id,creator,request_id,config) VALUES($1,$2,$3,$4,$5) ON CONFLICT(task_id,request_id) DO UPDATE SET request_id=EXCLUDED.request_id RETURNING `+jobColumns, task, exp, creator, c.RequestID, b))
}
func (s Store) Get(task string, id int64) (Job, error) {
	return scanJob(s.DB.QueryRow(`SELECT `+jobColumns+` FROM fuzz_jobs WHERE task_id=$1 AND id=$2`, task, id))
}
func (s Store) List(task string) ([]Job, error) {
	rows, err := s.DB.Query(`SELECT `+jobColumns+` FROM fuzz_jobs WHERE ($1='' OR task_id::text=$1) ORDER BY id DESC`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s Store) State(id int64, state, reason string) error {
	_, e := s.DB.Exec(`UPDATE fuzz_jobs SET state=$2,reason=$3 WHERE id=$1`, id, state, reason)
	return e
}
func (s Store) Pending(task string) bool {
	var yes bool
	err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM fuzz_jobs j WHERE task_id=$1 AND (state IN ('queued','preparing','running','stopping') OR EXISTS(SELECT 1 FROM fuzz_groups g WHERE g.job_id=j.id AND g.intent_id IS NULL AND (g.requested OR ((j.config->>'auto_analyze')::boolean AND j.auto_count<5)))))`, task).Scan(&yes)
	return err != nil || yes
}
func (s Store) Auto(task string, id int64, on bool) error {
	_, e := s.DB.Exec(`UPDATE fuzz_jobs SET config=jsonb_set(config,'{auto_analyze}',to_jsonb($3::boolean)) WHERE task_id=$1 AND id=$2`, task, id, on)
	return e
}
func (s Store) Groups(id int64) ([]Group, error) {
	rows, e := s.DB.Query(`SELECT g.id,g.job_id,g.signature,g.kind,g.sample,g.detail,g.count,COALESCE(g.intent_id,0),COALESCE(n.state,'pending') FROM fuzz_groups g LEFT JOIN exploration_nodes n ON n.id=g.intent_id WHERE g.job_id=$1 ORDER BY g.id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if e = rows.Scan(&g.ID, &g.JobID, &g.Signature, &g.Kind, &g.Sample, &g.Detail, &g.Count, &g.IntentID, &g.AnalysisState); e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s Store) RequestAnalysis(job, group int64) error {
	r, e := s.DB.Exec(`UPDATE fuzz_groups SET requested=true WHERE job_id=$1 AND id=$2`, job, group)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Apply atomically commits an event and its cursor. Replaying the stream is safe.
func (s Store) Apply(ctx context.Context, id int64, e Event) error {
	if e.Version != 1 || e.Seq < 1 {
		return fmt.Errorf("unsupported fuzz event")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRow(`SELECT cursor FROM fuzz_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&cursor); err != nil {
		return err
	}
	if e.Seq <= cursor {
		return nil
	}
	switch e.Type {
	case "state":
		if e.State != "running" && e.State != "completed" && e.State != "failed" {
			return fmt.Errorf("invalid runner state")
		}
		_, err = tx.Exec(`UPDATE fuzz_jobs SET state=$2,reason=$3 WHERE id=$1 AND state IN ('preparing','running')`, id, e.State, e.Reason)
	case "metrics":
		if !json.Valid(e.Metrics) {
			return fmt.Errorf("invalid metrics")
		}
		_, err = tx.Exec(`UPDATE fuzz_jobs SET metrics=$2 WHERE id=$1`, id, e.Metrics)
	case "sample":
		if !sampleName(e.Sample) || len(e.Detail) > 32000 || len(e.Signature) > 256 {
			return fmt.Errorf("invalid sample event")
		}
		var gid int64
		err = tx.QueryRow(`INSERT INTO fuzz_groups(job_id,signature,kind,sample,detail) VALUES($1,$2,$3,$4,$5) ON CONFLICT(job_id,signature) DO UPDATE SET signature=EXCLUDED.signature RETURNING id`, id, e.Signature, e.Kind, e.Sample, e.Detail).Scan(&gid)
		if err == nil {
			_, err = tx.Exec(`INSERT INTO fuzz_samples(job_id,sample,group_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, e.Sample, gid)
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE fuzz_groups SET count=(SELECT count(*) FROM fuzz_samples WHERE group_id=$1) WHERE id=$1`, gid)
		}
		if err == nil {
			_, err = tx.Exec(`INSERT INTO task_artifacts(task_id,name,kind,mime,sha256,note,storage_path)
 SELECT task_id::text,$2,'data','application/octet-stream',$3,$4,'tasks/'||task_id||'/shared/fuzz/'||id||'/samples/'||$3
 FROM fuzz_jobs j WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM task_artifacts a WHERE a.storage_path='tasks/'||j.task_id||'/shared/fuzz/'||j.id||'/samples/'||$3)`, id, "fuzz-sample-"+e.Sample, e.Sample, fmt.Sprintf("LibAFL job #%d group #%d; unverified candidate", id, gid))
		}
	default:
		return fmt.Errorf("unknown fuzz event type")
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE fuzz_jobs SET cursor=$2 WHERE id=$1`, id, e.Seq); err != nil {
		return err
	}
	return tx.Commit()
}

// Dispatch inserts the analysis intent and links it in one transaction. The graph
// itself is the durable queue; a crash between commit and Notify loses no intent.
func (s Store) Dispatch(j Job) (bool, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	var count int
	var ready bool
	if e = tx.QueryRow(`SELECT auto_count,last_analysis IS NULL OR last_analysis<now()-interval '1 minute' FROM fuzz_jobs WHERE id=$1 FOR UPDATE`, j.ID).Scan(&count, &ready); e != nil {
		return false, e
	}
	var gid int64
	var requested bool
	e = tx.QueryRow(`SELECT id,requested FROM fuzz_groups WHERE job_id=$1 AND intent_id IS NULL AND (requested OR $2) ORDER BY requested DESC,id LIMIT 1 FOR UPDATE`, j.ID, j.Config.AutoAnalyze && count < 5 && ready).Scan(&gid, &requested)
	if e == sql.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	payload, _ := json.Marshal(map[string]any{"text": fmt.Sprintf("分析 LibAFL 作业 #%d 的异常组 #%d。先调用 fuzz_results 获取证据，再 fuzz_replay 验证。检查 harness API 使用与源码版本，区分目标缺陷、harness/环境问题、未复现或证据不足。必要时最小化输入。仅确认目标漏洞后走现有 report_finding 与证据登记流程，并在报告注明 fuzz_job_id=%d、group_id=%d。", j.ID, gid, j.ID, gid), "fuzz_job_id": j.ID, "fuzz_group_id": gid})
	var iid int64
	e = tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'intent',$2,8,'open','fuzz') RETURNING id`, j.ExplorationID, payload).Scan(&iid)
	if e != nil {
		return false, e
	}
	if _, e = tx.Exec(`UPDATE fuzz_groups SET intent_id=$2 WHERE id=$1`, gid, iid); e != nil {
		return false, e
	}
	if !requested {
		if _, e = tx.Exec(`UPDATE fuzz_jobs SET auto_count=auto_count+1,last_analysis=now() WHERE id=$1`, j.ID); e != nil {
			return false, e
		}
	}
	return true, tx.Commit()
}
