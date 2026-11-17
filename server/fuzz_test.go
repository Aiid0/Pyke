package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aiid0/Pyke/fuzzing"
)

type fuzzFixtureRuntime struct{}

func (fuzzFixtureRuntime) Check(context.Context) (string, error)                    { return "fixture", nil }
func (fuzzFixtureRuntime) Start(context.Context, fuzzing.Job, string, string) error { return nil }
func (fuzzFixtureRuntime) Inspect(context.Context, fuzzing.Job) (bool, string, error) {
	return false, "container_missing", nil
}
func (fuzzFixtureRuntime) Stop(context.Context, fuzzing.Job) error { return nil }
func TestFuzzAPIRejectsGetMutations(t *testing.T) {
	s := &Server{}
	for _, op := range []string{"start", "stop", "auto", "analyze", "replay", "continue"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("action", op)
		w := httptest.NewRecorder()
		s.fuzzAPI(w, r)
		if w.Code != 405 {
			t.Fatalf("%s: %d %s", op, w.Code, w.Body.String())
		}
	}
}
func TestFuzzTaskServiceOwnershipAndPause(t *testing.T) {
	if os.Getenv("PYKE_PG_DSN") == "" {
		t.Skip("PG required")
	}
	m, e := NewManager(t.TempDir(), "")
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	task, e := m.CreateTask("fuzz HTTP fixture", "test", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{m: m, engine: NewEngine(m), ctx: t.Context()}
	store := fuzzing.Store{DB: m.pg}
	if e = store.Ensure(); e != nil {
		t.Fatal(e)
	}
	s.fuzz = &fuzzing.Manager{Root: m.dir, Store: store, Runtime: fuzzFixtureRuntime{}, Allowed: func(id string) bool { return !s.engine.IsPaused(id) }, Wake: func(string) {}}
	root := filepath.Join(m.dir, "tasks", task.ID, "shared", "fixture")
	if e = os.MkdirAll(filepath.Join(root, "seeds"), 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "build.sh"), []byte("true"), 0644); e != nil {
		t.Fatal(e)
	}
	raw := json.RawMessage(`{"source":"shared/fixture","build":"build.sh","seeds":"seeds","request_id":"http"}`)
	result, e := s.fuzzAction(t.Context(), task.ID, "start", "human", raw)
	if e != nil {
		t.Fatal(e)
	}
	j := result.(fuzzing.Job)
	_, e = s.fuzzAction(t.Context(), "99999999", "status", "human", json.RawMessage(`{"id":1}`))
	if e == nil {
		t.Fatal("cross task accepted")
	}
	s.engine.Pause(task.ID, nil)
	_, e = s.fuzzAction(t.Context(), task.ID, "start", "human", raw)
	if e == nil {
		t.Fatal("paused creation accepted")
	}
	if e = s.fuzz.Stop(t.Context(), task.ID, j.ID); e != nil {
		t.Fatal(e)
	}
}
