package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Aiid0/Pyke/db"
	"github.com/Aiid0/Pyke/intercept"
)

// MCP deferred 调用的解包回归：模型调 MCP 工具时，外层 tool_use 名字固定是
// ExecuteExtraTool（norma deferred.go 的包装工具），真正执行的是内层
// {tool_name, params}。v0.4.71 之前 guard 不解包，MCP 调用对三层闸门完全
// 隐身——vm_exec 这类高危能力等于裸奔。这里锁定解包后的行为。

// mcpCall 构造一次 ExecuteExtraUse 外层调用。
func mcpCall(inner string, params map[string]any) (string, []byte) {
	input, _ := json.Marshal(map[string]any{"tool_name": inner, "params": params})
	return "ExecuteExtraTool", input
}

// deny 规则按**内层工具名**命中：guard 必须解包后匹配。
func TestMCPUnwrappedNameHitsDenyRule(t *testing.T) {
	rules := []db.InterceptRule{
		{
			ID: 1, Name: "[内置] 靶机快照还原需人工确认", Enabled: true, Priority: 100,
			MatchTarget: "tool_name", MatchType: "contains",
			Pattern: "vm_snapshot_restore", Action: "deny",
			Message: "快照还原会清空靶机现场，需人工确认",
		},
	}
	ic := intercept.New(nil)
	ic.SetRulesInMemory(rules)
	g := NewWithInterceptor(ic)

	outer, input := mcpCall("mcp__vm-lab__vm_snapshot_restore", map[string]any{"vm": "win7-lab"})
	blocked, msg, _ := g.Hooks().PreToolUse(context.Background(), outer, input)
	if !blocked {
		t.Fatal("解包后的 MCP 工具应命中 deny 规则")
	}
	if msg == "" {
		t.Fatal("拦截消息不应为空")
	}

	// 审计里记的是**内层工具名**（外层 ExecuteExtraTool 无审计价值）。
	audit := g.Audit()
	if len(audit) == 0 || audit[len(audit)-1].Tool != "mcp__vm-lab__vm_snapshot_restore" {
		t.Fatalf("审计应记录内层工具名，实际 %+v", audit)
	}
}

// deny 规则也能按**内层 params** 命中：tool_input 匹配面必须是解包后的参数，
// 而不是外层的 {"tool_name":...} 包装。
func TestMCPParamsReachToolInputRule(t *testing.T) {
	rules := []db.InterceptRule{
		{
			ID: 1, Name: "[内置] 拒绝对 guest 执行格式化", Enabled: true, Priority: 100,
			MatchTarget: "tool_input", MatchType: "regex",
			Pattern: `\bformat\b|\bmkfs\b`, Action: "deny",
			Message: "禁止在 guest 内执行格式化",
		},
	}
	ic := intercept.New(nil)
	ic.SetRulesInMemory(rules)
	g := NewWithInterceptor(ic)

	outer, input := mcpCall("mcp__vm-lab__vm_exec", map[string]any{
		"vm": "win7-lab", "exe": "cmd", "args": "/c format D: /y",
	})
	blocked, _, _ := g.Hooks().PreToolUse(context.Background(), outer, input)
	if !blocked {
		t.Fatal("params 里的 format 应命中 tool_input deny 规则")
	}
}

// 无规则命中时不拦截、不调模型：只读类 MCP（如 FOFA 查询）行为不变。
func TestMCPNoRuleHitPassesThrough(t *testing.T) {
	judgeCalls := 0
	reviewer := func(ctx context.Context, profileID int64, prompt string, input intercept.ReviewInput) (intercept.Decision, error) {
		judgeCalls++
		return intercept.Decision{Action: "allow"}, nil
	}
	ic := intercept.New(nil)
	ic.SetReviewer(reviewer)
	g := NewWithInterceptor(ic)

	outer, input := mcpCall("mcp__fofa__get_alerts", map[string]any{"query": "app=tomcat"})
	blocked, _, _ := g.Hooks().PreToolUse(context.Background(), outer, input)
	if blocked {
		t.Fatal("无规则命中不应拦截")
	}
	if judgeCalls != 0 {
		t.Fatalf("judge 未启用场景下不应调用模型（nil reviewer 之外的计数）: %d", judgeCalls)
	}
}

// 外层包装畸形（缺 tool_name）时按原样放行，不 panic——同步路径上一崩就是
// 整个 agent 进程死掉。
func TestMCPUnwrapMalformedInputNoPanic(t *testing.T) {
	g := New()
	blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "ExecuteExtraTool", []byte(`{"params":{}}`))
	if blocked {
		t.Fatal("畸形输入不应拦截")
	}
	blocked, _, _ = g.Hooks().PreToolUse(context.Background(), "ExecuteExtraTool", []byte(`not-json`))
	if blocked {
		t.Fatal("非 JSON 输入不应拦截")
	}
}
