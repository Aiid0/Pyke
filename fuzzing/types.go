// Package fuzzing manages task-scoped, durable LibAFL campaigns.
package fuzzing

import (
	"encoding/json"
	"fmt"
	"time"
)

type Config struct {
	Source       string `json:"source"`
	Build        string `json:"build"`
	Seeds        string `json:"seeds"`
	Dictionary   string `json:"dictionary,omitempty"`
	Seconds      int    `json:"seconds"`
	BuildSeconds int    `json:"build_seconds"`
	Timeout      int    `json:"timeout"`
	MemoryMB     int    `json:"memory_mb"`
	CPUs         int    `json:"cpus"`
	MaxInput     int    `json:"max_input"`
	OutputMB     int    `json:"output_mb"`
	AutoAnalyze  bool   `json:"auto_analyze"`
	RequestID    string `json:"request_id"`
	ParentID     int64  `json:"parent_id,omitempty"`
	Replay       string `json:"replay,omitempty"`
}

func (c *Config) Defaults() error {
	if c.Seconds == 0 {
		c.Seconds = 1800
	}
	if c.BuildSeconds == 0 {
		c.BuildSeconds = 600
	}
	if c.Timeout == 0 {
		c.Timeout = 1
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 2048
	}
	if c.CPUs == 0 {
		c.CPUs = 1
	}
	if c.MaxInput == 0 {
		c.MaxInput = 1048576
	}
	if c.OutputMB == 0 {
		c.OutputMB = 1024
	}
	if c.Seconds < 1 || c.Seconds > 86400 || c.BuildSeconds < 1 || c.BuildSeconds > 3600 || c.Timeout < 1 || c.Timeout > 60 || c.MemoryMB < 256 || c.MemoryMB > 16384 || c.CPUs < 1 || c.CPUs > 4 || c.MaxInput < 1 || c.MaxInput > 16777216 || c.OutputMB < 16 || c.OutputMB > 4096 {
		return fmt.Errorf("fuzz resource budget outside allowed limits")
	}
	if c.Source == "" || c.Build == "" || c.Seeds == "" || len(c.RequestID) < 1 || len(c.RequestID) > 128 {
		return fmt.Errorf("source, build, seeds and request_id required")
	}
	return nil
}

type Job struct {
	ID            int64           `json:"id"`
	TaskID        string          `json:"task_id"`
	ExplorationID int64           `json:"-"`
	Creator       string          `json:"creator"`
	State         string          `json:"state"`
	Reason        string          `json:"reason"`
	Config        Config          `json:"config"`
	Image         string          `json:"image"`
	Snapshot      string          `json:"snapshot"`
	Metrics       json.RawMessage `json:"metrics"`
	Cursor        int64           `json:"cursor"`
	CreatedAt     time.Time       `json:"created_at"`
}
type Group struct {
	ID            int64  `json:"id"`
	JobID         int64  `json:"job_id"`
	Signature     string `json:"signature"`
	Kind          string `json:"kind"`
	Sample        string `json:"sample"`
	Detail        string `json:"detail"`
	Count         int    `json:"count"`
	IntentID      int64  `json:"intent_id"`
	AnalysisState string `json:"analysis_state"`
}
type Event struct {
	Version   int             `json:"version"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	State     string          `json:"state,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Metrics   json.RawMessage `json:"metrics,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Sample    string          `json:"sample,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

func Active(state string) bool {
	return state == "queued" || state == "preparing" || state == "running" || state == "stopping"
}
