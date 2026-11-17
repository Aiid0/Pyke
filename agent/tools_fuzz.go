package agent

import (
	"context"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
)

// FuzzCall is wired to the same service used by the task UI.
var FuzzCall func(context.Context, int64, string, json.RawMessage) (any, error)

func (t *ToolSet) fuzzTool(op string, read bool) actool.CoreTool {
	desc := map[string]string{
		"start":   "启动任务级 LibAFL 作业。source 是任务目录下的源码目录；build、seeds、dictionary 相对 source。build 脚本必须生成 $OUT/fuzz_target，链接 $LIBAFL_LIB。调用前加载 fuzz-workflow skill。跨 Agent 回合运行；不要紧密轮询。",
		"list":    "列出当前任务的 Fuzz 作业。",
		"status":  "读取 Fuzz 作业状态、预算和指标。",
		"results": "读取 Fuzz 异常组、分析意图和任务相对证据目录。crash 候选不等于确认漏洞。",
		"stop":    "停止 Fuzz 作业及全部容器子进程。",
		"replay":  "为异常组创建独立回放作业，返回新作业 ID。",
		"analyze": "为异常组请求 Worker 分析；重复请求复用已关联意图。",
	}
	props := map[string]any{"id": map[string]any{"type": "integer", "description": "作业 ID"}, "group_id": map[string]any{"type": "integer", "description": "异常组 ID"}, "request_id": str("每次新作业唯一的幂等 ID")}
	required := []string{"id"}
	if op == "list" {
		required = nil
	}
	if op == "replay" || op == "analyze" {
		required = append(required, "group_id")
	}
	if op == "replay" {
		required = append(required, "request_id")
	}
	if op == "start" {
		props = map[string]any{"source": str("任务相对源码目录"), "build": str("源码内构建脚本路径"), "seeds": str("源码内种子目录"), "dictionary": str("可选字典路径"), "request_id": str("幂等 ID"), "auto_analyze": map[string]any{"type": "boolean", "description": "自动分析新异常，默认关闭"}}
		for _, key := range []string{"seconds", "build_seconds", "timeout", "memory_mb", "cpus", "max_input", "output_mb"} {
			props[key] = map[string]any{"type": "integer"}
		}
		required = []string{"source", "build", "seeds", "request_id"}
	}
	call := func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if FuzzCall == nil || t.taskID <= 0 {
			return actool.Errorf("Fuzz 服务或任务上下文不可用"), nil
		}
		out, e := FuzzCall(ctx, t.taskID, op, in)
		if e != nil {
			return actool.Errorf(e.Error()), nil
		}
		return jsonResult(out)
	}
	if read {
		return readTool("fuzz_"+op, desc[op], obj(props, required...), call)
	}
	return writeTool("fuzz_"+op, desc[op], obj(props, required...), call)
}
