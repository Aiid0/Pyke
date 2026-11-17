package fuzzing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aiid0/Pyke/db"
)

type fakeRuntime struct {
	starts, stops int
	alive         bool
}

func (f *fakeRuntime) Check(context.Context) (string, error) { return "sha256:fixture", nil }
func (f *fakeRuntime) Start(context.Context, Job, string, string) error {
	f.starts++
	f.alive = true
	return nil
}
func (f *fakeRuntime) Inspect(context.Context, Job) (bool, string, error) {
	return f.alive, "container_missing", nil
}
func (f *fakeRuntime) Stop(context.Context, Job) error { f.stops++; f.alive = false; return nil }

func TestSnapshotRejectsSymlinksAndPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if e := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(root, "escape")); e != nil {
		t.Skip(e)
	}
	if _, e := inside(root, "escape/secret"); e == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, e := copySnapshot(root, filepath.Join(t.TempDir(), "snapshot")); e == nil {
		t.Fatal("symlink snapshot accepted")
	}
	if _, e := inside(root, "../secret"); e == nil {
		t.Fatal("parent traversal accepted")
	}
}
func TestBudgetValidation(t *testing.T) {
	c := Config{Source: "src", Build: "build.sh", Seeds: "seeds", RequestID: "test"}
	if e := c.Defaults(); e != nil {
		t.Fatal(e)
	}
	if c.Seconds != 1800 || c.MemoryMB != 2048 {
		t.Fatal(c)
	}
	c.CPUs = -1
	if c.Defaults() == nil {
		t.Fatal("negative cpu allowed")
	}
}

func TestAutoDiscoverCreatesIdempotentJob(t *testing.T) {
	s, task, exp := testStore(t)
	root := t.TempDir()
	target := filepath.Join(root, "tasks", task, "shared", "fuzz-targets", "parser")
	if err := os.MkdirAll(filepath.Join(target, "seeds"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"build.sh":       "#!/bin/sh\ntrue\n",
		"seeds/initial":  "{}",
		"pyke-fuzz.json": `{"source":"shared/fuzz-targets/parser","build":"build.sh","seeds":"seeds","auto_analyze":true}`,
	} {
		if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	rt := &fakeRuntime{}
	m := &Manager{Store: s, Root: root, Runtime: rt}
	jobs, err := m.AutoDiscover(t.Context(), task, exp)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("auto discovery: jobs=%d err=%v", len(jobs), err)
	}
	if jobs[0].Creator != "autopilot" || !jobs[0].Config.AutoAnalyze {
		t.Fatalf("unexpected auto job: %+v", jobs[0])
	}
	again, err := m.AutoDiscover(t.Context(), task, exp)
	if err != nil || len(again) != 1 || again[0].ID != jobs[0].ID {
		t.Fatalf("auto discovery not idempotent: %+v err=%v", again, err)
	}
}

func testStore(t *testing.T) (Store, string, int64) {
	t.Helper()
	dsn := os.Getenv("PYKE_PG_DSN")
	if dsn == "" {
		t.Skip("PYKE_PG_DSN required")
	}
	pg, e := db.Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pg.Close() })
	s := Store{pg}
	if e = s.Ensure(); e != nil {
		t.Fatal(e)
	}
	exp, e := pg.CreateExploration("fuzz fixture", "test")
	if e != nil {
		t.Fatal(e)
	}
	var id int64
	if e = pg.QueryRow(`INSERT INTO tasks(description,goal,exploration_id) VALUES('fuzz fixture','test',$1) RETURNING id`, exp).Scan(&id); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tasks WHERE id=$1`, id)
		_, _ = pg.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	})
	return s, strconv.FormatInt(id, 10), exp
}
func TestDurableCampaignAndAnalysis(t *testing.T) {
	s, task, exp := testStore(t)
	root := t.TempDir()
	src := filepath.Join(root, "tasks", task, "shared", "parser")
	if e := os.MkdirAll(filepath.Join(src, "seeds"), 0755); e != nil {
		t.Fatal(e)
	}
	for p, b := range map[string]string{"build.sh": "true", "seeds/seed": "abc"} {
		if e := os.WriteFile(filepath.Join(src, p), []byte(b), 0644); e != nil {
			t.Fatal(e)
		}
	}
	rt := &fakeRuntime{}
	allowed := true
	m := &Manager{Store: s, Root: root, Runtime: rt, Allowed: func(string) bool { return allowed }}
	c := Config{Source: "shared/parser", Build: "build.sh", Seeds: "seeds", RequestID: "one", AutoAnalyze: true}
	j, e := m.Create(t.Context(), task, exp, "test", c)
	if e != nil {
		t.Fatal(e)
	}
	again, e := m.Create(t.Context(), task, exp, "test", c)
	if e != nil || again.ID != j.ID {
		t.Fatal("idempotency", again, e)
	}
	c.RequestID = "escape"
	c.Source = "."
	if _, e = m.Create(t.Context(), task, exp, "test", c); e == nil {
		t.Fatal("recursive source accepted")
	}
	m.Tick(t.Context())
	if rt.starts != 1 {
		t.Fatal("did not start")
	}
	if !s.Pending(task) {
		t.Fatal("active job not pending")
	}
	ev := Event{Version: 1, Seq: 1, Type: "sample", Kind: "crash_candidate", Signature: "fixture", Sample: strings.Repeat("a", 64), Detail: "stack"}
	if e = s.Apply(t.Context(), j.ID, ev); e != nil {
		t.Fatal(e)
	}
	if e = s.Apply(t.Context(), j.ID, ev); e != nil {
		t.Fatal(e)
	}
	groups, e := s.Groups(j.ID)
	if e != nil || len(groups) != 1 || groups[0].Count != 1 {
		t.Fatal(groups, e)
	}
	allowed = false
	m.Tick(t.Context())
	groups, _ = s.Groups(j.ID)
	if groups[0].IntentID != 0 {
		t.Fatal("paused task dispatched analysis")
	}
	allowed = true
	m.Tick(t.Context())
	groups, _ = s.Groups(j.ID)
	if groups[0].IntentID == 0 {
		t.Fatal("analysis missing")
	}
	iid := groups[0].IntentID
	m.Tick(t.Context())
	groups, _ = s.Groups(j.ID)
	if groups[0].IntentID != iid {
		t.Fatal("duplicate analysis")
	}
	if e = m.Stop(t.Context(), task, j.ID); e != nil {
		t.Fatal(e)
	}
	if rt.alive {
		t.Fatal("stop left runtime alive")
	}
	if _, e = s.Get("99999999", j.ID); e == nil {
		t.Fatal("cross task read")
	}
}
func TestEventsIgnorePartialTailAndRecover(t *testing.T) {
	s, task, exp := testStore(t)
	c := Config{RequestID: "events"}
	j, e := s.Create(task, exp, "test", c)
	if e != nil {
		t.Fatal(e)
	}
	m := Manager{Store: s, Root: t.TempDir()}
	if e = os.MkdirAll(m.Dir(j), 0755); e != nil {
		t.Fatal(e)
	}
	ev := Event{Version: 1, Seq: 1, Type: "metrics", Metrics: json.RawMessage(`{"coverage":12}`)}
	b, _ := json.Marshal(ev)
	b = append(b, '\n')
	b = append(b, []byte(`{"version":1`)...)
	if e = os.WriteFile(filepath.Join(m.Dir(j), "events.jsonl"), b, 0644); e != nil {
		t.Fatal(e)
	}
	if e = m.collect(t.Context(), j); e != nil {
		t.Fatal(e)
	}
	fresh, e := s.Get(task, j.ID)
	if e != nil || fresh.Cursor != 1 {
		t.Fatal(fresh, e)
	}
}

func TestAutoAnalysisLimitAndManualOverride(t *testing.T) {
	s, task, exp := testStore(t)
	c := Config{RequestID: "limits", AutoAnalyze: true}
	j, e := s.Create(task, exp, "test", c)
	if e != nil {
		t.Fatal(e)
	}
	for i := int64(1); i <= 7; i++ {
		sha := strings.Repeat("0", 63) + strconv.FormatInt(i, 16)
		if e = s.Apply(t.Context(), j.ID, Event{Version: 1, Seq: i, Type: "sample", Kind: "crash_candidate", Signature: sha, Sample: sha}); e != nil {
			t.Fatal(e)
		}
	}
	if ok, e := s.Dispatch(j); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := s.Dispatch(j); e != nil || ok {
		t.Fatal("rate limit", ok, e)
	}
	for i := 0; i < 4; i++ {
		_, _ = s.DB.Exec(`UPDATE fuzz_jobs SET last_analysis=now()-interval '2 minutes' WHERE id=$1`, j.ID)
		if ok, e := s.Dispatch(j); e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	_, _ = s.DB.Exec(`UPDATE fuzz_jobs SET last_analysis=now()-interval '2 minutes' WHERE id=$1`, j.ID)
	if ok, e := s.Dispatch(j); e != nil || ok {
		t.Fatal("cap", ok, e)
	}
	groups, _ := s.Groups(j.ID)
	if e = s.RequestAnalysis(j.ID, groups[5].ID); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.Dispatch(j); e != nil || !ok {
		t.Fatal("manual request", ok, e)
	}
}
