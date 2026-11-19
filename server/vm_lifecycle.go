package server

// Task-owned VirtualBox lifecycle management.  vm-lab itself owns the driver
// details (local/WSL/SSH); this layer owns the only policy that matters to Pyke:
// a VM that Pyke actually started for a task must not survive that task silently.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Aiid0/Pyke/db"
)

const (
	vmLeaseFileName    = "vm-leases.json"
	vmStopGraceDefault = 30 * time.Second
	vmStopTimeout      = 90 * time.Second
)

type vmLifecycle struct {
	root  string
	pg    *db.DB
	grace time.Duration
}

func newVMLifecycle(root string, pg *db.DB) *vmLifecycle {
	return &vmLifecycle{root: root, pg: pg, grace: vmStopGraceDefault}
}

func (v *vmLifecycle) leaseFile(taskID int64) string {
	return filepath.Join(v.root, "tasks", strconv.FormatInt(taskID, 10), "runtime", vmLeaseFileName)
}

// injectVMLifecycleEnv gives the one task-scoped vm-lab subprocess a lease path.
// It must never modify the DB-owned MCP config: the same MCP can serve concurrent
// tasks, each with its own process and lease file.
func (v *vmLifecycle) injectVMLifecycleEnv(m *db.MCPServer, taskID int64) *db.MCPServer {
	if v == nil || m == nil || m.Name != "vm-lab" || taskID <= 0 {
		return m
	}
	clone := *m
	env := jsonStrMap(m.Env)
	env["PYKE_VM_LEASE_FILE"] = v.leaseFile(taskID)
	b, _ := json.Marshal(env)
	clone.Env = b
	return &clone
}

// StopTask cleans only leases written by vm_start after it booted a VM.  A VM
// merely observed as already running is never in a lease and is therefore never
// touched.  On cleanup failure the lease remains on disk for retry/audit.
func (v *vmLifecycle) StopTask(ctx context.Context, taskID string) error {
	if v == nil || v.pg == nil {
		return nil
	}
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("invalid VM lifecycle task id %q", taskID)
	}
	lease := v.leaseFile(id)
	if _, err := os.Stat(lease); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	servers, err := v.pg.ListMCP()
	if err != nil {
		return fmt.Errorf("list MCP servers: %w", err)
	}
	var m *db.MCPServer
	for _, candidate := range servers {
		if candidate.Name == "vm-lab" && candidate.Transport == "stdio" {
			m = candidate
			break
		}
	}
	if m == nil {
		return fmt.Errorf("vm-lab MCP 配置不可用；保留租约 %s 以便恢复后回收", lease)
	}
	if m.Command == "" {
		return fmt.Errorf("vm-lab MCP 缺少启动命令；保留租约等待重试")
	}
	args := append([]string{}, jsonStrSlice(m.Args)...)
	args = append(args, "--pyke-cleanup-leases", lease, "--grace-seconds", strconv.Itoa(int(v.grace.Seconds())))
	cmd := exec.CommandContext(ctx, m.Command, args...)
	cmd.Dir = v.root
	cmd.Env = append(os.Environ(), envPairs(jsonStrMap(m.Env))...)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return fmt.Errorf("vm-lab 回收失败: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		log.Printf("[vm] task %s cleanup: %s", taskID, msg)
	}
	return nil
}

func (v *vmLifecycle) StopAll(ctx context.Context) {
	if v == nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(v.root, "tasks"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := v.StopTask(ctx, entry.Name()); err != nil {
			log.Printf("[vm] shutdown cleanup task %s: %v", entry.Name(), err)
		}
	}
}

// Run provides the process-exit recovery leg.  Normal task controls call
// StopTask synchronously; this path catches service restarts and crashes that
// later unwind the server context.
func (v *vmLifecycle) Run(ctx context.Context) {
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), vmStopTimeout)
	defer cancel()
	v.StopAll(stopCtx)
}

func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for k, value := range env {
		pairs = append(pairs, k+"="+value)
	}
	return pairs
}
