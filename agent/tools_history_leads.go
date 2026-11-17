package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) searchHistoryLeads() actool.CoreTool {
	return t.readExpTool("search_history_leads", "只读检索本任务固定源码版本可达的历史修复线索。优先参考已分析的候选；评分与模型结论均需在当前源码中核实。", obj(map[string]any{
		"limit":  intp("每页条数，默认 5，最多 20"),
		"offset": intp("分页位置，从 0 开始"),
		"q":      str("提交说明或改动路径关键词，可选"),
	}), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
			Q      string `json:"q"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		items, err := t.ts.SearchTaskHistoryLeads(ctx, a.Limit, a.Offset, a.Q)
		if err != nil {
			return actool.Errorf("历史线索读取失败"), nil
		}
		return jsonResult(map[string]any{"items": items, "offset": a.Offset, "source_leads_only": true})
	})
}

func (t *ToolSet) getHistoryLead() actool.CoreTool {
	return t.readExpTool("get_history_lead", "读取本任务固定源码版本可达的提交详情、已有项目级深查结论和有界差异摘录。只作为验证方向。", obj(map[string]any{
		"commit_sha": str("完整提交 SHA"),
	}, "commit_sha"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			SHA string `json:"commit_sha"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		lead, analyses, err := t.ts.GetTaskHistoryLead(ctx, a.SHA)
		if err != nil {
			return actool.Errorf("历史线索读取失败"), nil
		}
		if lead == nil {
			return actool.Errorf("提交未索引或不属于当前任务源码版本"), nil
		}
		return jsonResult(map[string]any{"candidate": lead, "analyses": analyses, "source_lead_only": true})
	})
}
