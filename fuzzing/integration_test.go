package fuzzing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in real container test. The daemon must see this process's temp directory
// at the same path (mount /tmp when invoking from a Go build container).
func TestLibAFLContainer(t *testing.T) {
	if os.Getenv("PYKE_FUZZ_INTEGRATION") != "1" {
		t.Skip("set PYKE_FUZZ_INTEGRATION=1 and build runner image")
	}
	s, task, exp := testStore(t)
	root := t.TempDir()
	runtime := Docker{Root: root, Image: "pyke-libafl:0.15.4"}
	m := &Manager{Store: s, Root: root, Runtime: runtime}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 45*time.Second)
		defer c()
		_ = m.StopTask(ctx, task)
	})
	example := filepath.Join(root, "tasks", task, "shared", "fixture")
	if _, e := copySnapshot("runner/example", example); e != nil {
		t.Fatal(e)
	}
	run := func(c Config) Job {
		t.Helper()
		j, e := m.Create(t.Context(), task, exp, "integration", c)
		if e != nil {
			t.Fatal(e)
		}
		deadline := time.Now().Add(150 * time.Second)
		for time.Now().Before(deadline) {
			m.Tick(t.Context())
			j, e = s.Get(task, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			if !Active(j.State) {
				log, _ := m.Logs(j, 0)
				t.Logf("job %d state=%s reason=%s log=%v", j.ID, j.State, j.Reason, log)
				return j
			}
			select {
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			case <-time.After(time.Second):
			}
		}
		t.Fatal("container deadline exceeded")
		return j
	}
	j := run(Config{Source: "shared/fixture", Build: "build.sh", Seeds: "seeds", RequestID: "crash", Seconds: 5, BuildSeconds: 90})
	if j.State != "completed" {
		t.Fatalf("campaign failed: %s", j.Reason)
	}
	groups, e := s.Groups(j.ID)
	if e != nil || len(groups) == 0 {
		t.Fatalf("expected real crash artifact: %v %v", groups, e)
	}
	sample, err := os.ReadFile(filepath.Join(m.Dir(j), "samples", groups[0].Sample))
	if err != nil || !strings.Contains(string(sample), "PYKE") {
		t.Fatal("missing reproducer", err)
	}
	replay, e := m.Continue(t.Context(), task, j.ID, "replay", groups[0].Sample)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		m.Tick(t.Context())
		replay, e = s.Get(task, replay.ID)
		if e != nil {
			t.Fatal(e)
		}
		if !Active(replay.State) {
			break
		}
		time.Sleep(time.Second)
	}
	rg, e := s.Groups(replay.ID)
	if e != nil || len(rg) == 0 {
		log, _ := m.Logs(replay, 0)
		t.Fatalf("replay did not reproduce: %v %s %v", e, replay.State, log)
	}
	build := filepath.Join(example, "build.sh")
	b, e := os.ReadFile(build)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(build, []byte(strings.Replace(string(b), "${CXXFLAGS}", "${CXXFLAGS} -DPYKE_SAFE_FIXTURE", 1)), 0644); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(example, "seeds", "initial"), []byte("SAFE"), 0644); e != nil {
		t.Fatal(e)
	}
	safe := run(Config{Source: "shared/fixture", Build: "build.sh", Seeds: "seeds", RequestID: "safe", Seconds: 5, BuildSeconds: 90})
	gs, e := s.Groups(safe.ID)
	if e != nil || safe.State != "completed" || len(gs) != 0 {
		t.Fatal("safe target misclassified", safe.State, gs, e)
	}
	// Ensure corpus and real execution feedback exist, not just process startup.
	if string(safe.Metrics) == "{}" {
		t.Fatal("no feedback")
	}
}
