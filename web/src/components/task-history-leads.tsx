"use client";

import * as React from "react";
import { GitCommitHorizontalIcon } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { http } from "@/lib/api";

type Lead = { commit_sha: string; subject: string; score: number; reasons: string[]; depth: string; current_summary?: string };

export function TaskHistoryLeads({ taskId, projectId }: { taskId: string; projectId: string }) {
  const [items, setItems] = React.useState<Lead[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    const load = () => { void http<{ items: Lead[] }>(`/tasks/${encodeURIComponent(taskId)}/history-leads`)
      .then((result) => { if (active) setItems(result.items); })
      .catch(() => {})
      .finally(() => { if (active) setLoaded(true); }); };
    load();
    const timer = window.setInterval(load, 15000);
    return () => { active = false; window.clearInterval(timer); };
  }, [taskId]);

  return <Card>
    <CardHeader><CardTitle className="flex items-center gap-2 text-base"><GitCommitHorizontalIcon className="size-4 text-primary" />历史修复线索</CardTitle></CardHeader>
    <CardContent className="space-y-3 text-sm">
      {items.length > 0 ? items.map((item) => <div key={item.commit_sha} className="rounded-md border p-3">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1"><code className="text-xs text-primary">{item.commit_sha.slice(0, 10)}</code><span className="font-medium">{item.subject}</span>
          <span className="text-xs text-muted-foreground">{item.depth === "deep" ? "已深查" : "规则筛选"} · 分数 {item.score}</span></div>
        <p className="mt-1 line-clamp-3 text-xs text-muted-foreground">{item.current_summary || item.reasons.join(" · ")}</p>
      </div>) : <p className="text-xs text-muted-foreground">{loaded ? "当前源码版本暂无已索引线索；可在历史分析中开启或继续同步。" : "正在查找当前版本的历史线索…"}</p>}
      <p className="text-xs text-muted-foreground">这些提交是待核查的源码方向，Planner 会优先参考与任务目标相关的少量线索。</p>
      <a href={`/function/history?project=${encodeURIComponent(projectId)}`} className="inline-flex text-xs font-medium text-primary hover:underline">查看仓库历史与完整分析</a>
    </CardContent>
  </Card>;
}
