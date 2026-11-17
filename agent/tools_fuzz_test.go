package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFuzzToolBindingsAndExecutionGate(t *testing.T) {
	expected := map[string][]string{"fuzz_start": {"worker"}, "fuzz_replay": {"worker"}, "fuzz_status": {"mainagent", "planner", "worker"}, "fuzz_analyze": {"mainagent", "worker"}}
	for _, seed := range BuiltinToolSeeds() {
		want, ok := expected[seed.Key]
		if !ok {
			continue
		}
		if len(seed.Agents) != len(want) {
			t.Fatalf("%s bindings %v", seed.Key, seed.Agents)
		}
		for i := range want {
			if seed.Agents[i] != want[i] {
				t.Fatalf("%s bindings %v", seed.Key, seed.Agents)
			}
		}
		delete(expected, seed.Key)
	}
	if len(expected) != 0 {
		t.Fatal("missing tools", expected)
	}
	ts := NewToolSet(nil, "")
	filtered := DropLocalExecTools(ts.WorkerTools())
	for _, tool := range filtered {
		if tool.Name() == "fuzz_start" || tool.Name() == "fuzz_replay" {
			t.Fatal("execution gate bypass", tool.Name())
		}
	}
}
func TestFuzzToolPreservesTaskContext(t *testing.T) {
	old := FuzzCall
	t.Cleanup(func() { FuzzCall = old })
	ts := NewToolSet(nil, "")
	ts.SetTaskID(42)
	called := false
	FuzzCall = func(_ context.Context, task int64, op string, in json.RawMessage) (any, error) {
		called = true
		if task != 42 || op != "status" {
			t.Fatal(task, op)
		}
		return map[string]string{"state": "running"}, nil
	}
	result, err := ts.fuzzTool("status", true).Call(t.Context(), json.RawMessage(`{"id":3}`), nil)
	if err != nil || result.IsError || !called {
		t.Fatal(result, err)
	}
}
