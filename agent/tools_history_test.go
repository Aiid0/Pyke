package agent

import (
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestHistoryToolsRegisteredForTaskRoles(t *testing.T) {
	ts := NewToolSet(nil, "")
	want := []string{"search_fix_candidates", "get_fix_candidate", "get_commit_diff", "record_fix_analysis", "set_fix_lead_verdict"}
	for role, tools := range map[string][]actool.CoreTool{"mainagent": ts.MainAgentTools(), "planner": ts.PlannerTools(), "worker": ts.WorkerTools()} {
		names := map[string]bool{}
		for _, tool := range tools {
			names[tool.Name()] = true
		}
		for _, name := range want {
			if !names[name] {
				t.Errorf("%s missing %s", role, name)
			}
		}
	}
	seeds := map[string]ToolSeed{}
	for _, seed := range BuiltinToolSeeds() {
		seeds[seed.Key] = seed
	}
	for _, name := range want {
		if len(seeds[name].Agents) != 3 {
			t.Errorf("%s bindings=%v", name, seeds[name].Agents)
		}
	}
}
