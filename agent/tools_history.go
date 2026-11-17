package agent

import (
	"context"
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) searchFixCandidates() actool.CoreTool {
	return t.readExpTool("search_fix_candidates", "分页查本任务固定源码提交可达的历史修复候选；返回的是待验证线索，不代表漏洞。先选择少量高优先级线索，再用 get_fix_candidate 查看详情。", obj(map[string]any{
		"limit": intp("每页条数，默认 20，最多 50"), "offset": intp("从 0 开始的分页位置"), "q": str("提交说明或改动路径关键词，可选"),
	}), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
			Q      string `json:"q"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		items, err := t.ts.SearchTaskFixLeads(a.Limit, a.Offset, a.Q)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"items": items, "offset": a.Offset, "source_leads_only": true})
	})
}

func (t *ToolSet) getFixCandidate() actool.CoreTool {
	return t.readExpTool("get_fix_candidate", "按 SHA 读取本任务目标版本可达的修复候选、筛选理由及之前的分析轮次。", obj(map[string]any{"commit_sha": str("完整提交 SHA")}, "commit_sha"), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			SHA string `json:"commit_sha"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		lead, err := t.ts.GetTaskFixLead(a.SHA)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if lead == nil {
			return actool.Errorf("提交未索引或不适用于当前任务版本"), nil
		}
		analyses, err := t.ts.TaskFixAnalyses(a.SHA)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		lead.DiffExcerpt = ""
		return jsonResult(map[string]any{"candidate": lead, "analyses": analyses, "source_lead_only": true})
	})
}

func (t *ToolSet) getCommitDiff() actool.CoreTool {
	return t.readExpTool("get_commit_diff", "按 SHA 读取已索引候选的有界代码差异摘录；提交须属于当前任务目标版本，超过 64 KiB 的差异会截断。", obj(map[string]any{"commit_sha": str("完整提交 SHA")}, "commit_sha"), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			SHA string `json:"commit_sha"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		lead, err := t.ts.GetTaskFixLead(a.SHA)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if lead == nil {
			return actool.Errorf("提交未索引或不适用于当前任务版本"), nil
		}
		return jsonResult(map[string]any{"commit_sha": lead.CommitSHA, "diff": lead.DiffExcerpt, "truncated": lead.DiffTruncated, "source_lead_only": true})
	})
}

func (t *ToolSet) recordFixAnalysis() actool.CoreTool {
	return t.writeExpTool("record_fix_analysis", "保存当前任务对某一历史修复线索的新一轮源码分析。必须写清问题、结论和可复查依据；这不会把线索登记为确认漏洞，验证结果另用 record_fact/report_finding 留痕。", obj(map[string]any{
		"commit_sha": str("完整提交 SHA"), "question": str("本轮检查的具体问题"), "conclusion": str("分析结论及未决点"), "evidence": map[string]any{"type": "object", "description": "代码位置、反证和验证输出等结构化依据"},
	}, "commit_sha", "question", "conclusion"), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			SHA        string          `json:"commit_sha"`
			Question   string          `json:"question"`
			Conclusion string          `json:"conclusion"`
			Evidence   json.RawMessage `json:"evidence"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		if strings.TrimSpace(a.Conclusion) == "" {
			return actool.Errorf("结论不能为空"), nil
		}
		id, err := t.ts.RecordTaskFixAnalysis(a.SHA, a.Question, a.Conclusion, a.Evidence)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"analysis_id": id, "state": "analyzed"})
	})
}

func (t *ToolSet) setFixLeadVerdict() actool.CoreTool {
	return t.writeExpTool("set_fix_lead_verdict", "保存历史修复候选在本任务、当前源码版本的独立结论。reported 必须引用已通过 report_finding 登记的本任务漏洞 ID；ruled_out 需写反证。项目级候选和其他任务结论不受影响。", obj(map[string]any{
		"commit_sha": str("完整提交 SHA"), "state": str("unverified、ruled_out 或 reported"), "note": str("依据，至少 10 个字节"), "finding_id": intp("仅 reported 时填写本任务已登记的漏洞 ID"),
	}, "commit_sha", "state", "note"), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			SHA       string `json:"commit_sha"`
			State     string `json:"state"`
			Note      string `json:"note"`
			FindingID int64  `json:"finding_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("参数无效"), nil
		}
		if err := t.ts.SetTaskFixVerdict(a.SHA, a.State, a.Note, a.FindingID); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"commit_sha": a.SHA, "verification_state": a.State})
	})
}
