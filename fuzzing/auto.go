package fuzzing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AutoSpec is the small hand-off contract between an Agent preparing a target
// and the durable Fuzz scheduler. The Agent writes pyke-fuzz.json; the service
// discovers it and starts the job without requiring a second user action.
type AutoSpec struct {
	Source       string `json:"source"`
	Build        string `json:"build"`
	Seeds        string `json:"seeds"`
	Dictionary   string `json:"dictionary,omitempty"`
	RequestID    string `json:"request_id"`
	AutoAnalyze  bool   `json:"auto_analyze"`
	Seconds      int    `json:"seconds,omitempty"`
	BuildSeconds int    `json:"build_seconds,omitempty"`
	Timeout      int    `json:"timeout,omitempty"`
	MemoryMB     int    `json:"memory_mb,omitempty"`
	CPUs         int    `json:"cpus,omitempty"`
	MaxInput     int    `json:"max_input,omitempty"`
	OutputMB     int    `json:"output_mb,omitempty"`
}

func (m *Manager) AutoDiscover(ctx context.Context, task string, exp int64) ([]Job, error) {
	if m.Runtime == nil || m.Allowed != nil && !m.Allowed(task) {
		return nil, nil
	}
	if _, err := m.Runtime.Check(ctx); err != nil {
		return nil, nil // unavailable is visible in the host health card; do not spam failures
	}
	base := filepath.Join(m.Root, "tasks", task, "shared", "fuzz-targets")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var started []Job
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		marker := filepath.Join(base, entry.Name(), "pyke-fuzz.json")
		b, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		var spec AutoSpec
		if err := json.Unmarshal(b, &spec); err != nil {
			continue
		}
		if strings.TrimSpace(spec.RequestID) == "" {
			spec.RequestID = "auto-" + entry.Name()
		}
		cfg := Config{Source: spec.Source, Build: spec.Build, Seeds: spec.Seeds, Dictionary: spec.Dictionary,
			RequestID: spec.RequestID, AutoAnalyze: spec.AutoAnalyze, Seconds: spec.Seconds,
			BuildSeconds: spec.BuildSeconds, Timeout: spec.Timeout, MemoryMB: spec.MemoryMB,
			CPUs: spec.CPUs, MaxInput: spec.MaxInput, OutputMB: spec.OutputMB}
		job, err := m.Create(ctx, task, exp, "autopilot", cfg)
		if err != nil {
			return started, fmt.Errorf("auto fuzz %s: %w", entry.Name(), err)
		}
		started = append(started, job)
	}
	return started, nil
}
