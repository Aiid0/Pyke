package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Aiid0/Pyke/db"
)

func TestVMLifecycleInjectsTaskPrivateLeaseOnlyForVMLab(t *testing.T) {
	v := newVMLifecycle(t.TempDir(), nil)
	base := &db.MCPServer{Name: "vm-lab", Env: json.RawMessage(`{"VM_DRIVER":"ssh"}`)}
	configured := v.injectVMLifecycleEnv(base, 42)
	if configured == base {
		t.Fatal("vm-lab config should be cloned, never mutate DB-owned config")
	}
	env := jsonStrMap(configured.Env)
	if got, want := env["PYKE_VM_LEASE_FILE"], filepath.Join(v.root, "tasks", "42", "runtime", vmLeaseFileName); got != want {
		t.Fatalf("lease path = %q, want %q", got, want)
	}
	if jsonStrMap(base.Env)["PYKE_VM_LEASE_FILE"] != "" {
		t.Fatal("base MCP env was mutated")
	}
	other := &db.MCPServer{Name: "browser", Env: json.RawMessage(`{"X":"y"}`)}
	if got := v.injectVMLifecycleEnv(other, 42); got != other {
		t.Fatal("non-VM MCP must not receive a VM lifecycle env")
	}
}
