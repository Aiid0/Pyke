package agent

import (
	"slices"
	"testing"
)

func TestHistoryLeadToolsReachHuntingAgents(t *testing.T) {
	want := map[string]bool{"search_history_leads": false, "get_history_lead": false}
	for _, seed := range BuiltinToolSeeds() {
		if _, ok := want[seed.Key]; !ok {
			continue
		}
		for _, role := range []string{"planner", "worker", "mainagent"} {
			if !slices.Contains(seed.Agents, role) {
				t.Fatalf("%s missing %s binding", seed.Key, role)
			}
		}
		want[seed.Key] = true
	}
	for key, found := range want {
		if !found {
			t.Fatalf("%s missing from built-in catalog", key)
		}
	}
}
