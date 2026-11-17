"use client";

import * as React from "react";
import { GitCommitHorizontalIcon, Loader2Icon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { http } from "@/lib/api";
import type { ImportedProject } from "@/lib/types";

type Status = { state: string; head_sha: string; branch: string; indexed: number; candidates: number; pending: number; last_error: string; shallow: boolean; rewritten: boolean };
type Commit = { commit_sha: string; authored_at: string; subject: string; files: string[]; score: number; reasons: string[]; orphaned: boolean; candidate_id?: number; candidate_state?: string; depth?: string };
type Analysis = { id: number; question: string; conclusion: string; state: string; created_at: string; last_error?: string };

export function ProjectHistoryPanel({ project }: { project: ImportedProject }) {
  const [status, setStatus] = React.useState<Status | null>(null);
  const [commits, setCommits] = React.useState<Commit[]>([]);
  const [listError, setListError] = React.useState("");
  const [offset, setOffset] = React.useState(0);
  const [selected, setSelected] = React.useState<Commit | null>(null);
  const [diff, setDiff] = React.useState("");
  const [diffTruncated, setDiffTruncated] = React.useState(false);
  const [analyses, setAnalyses] = React.useState<Analysis[]>([]);
  const [question, setQuestion] = React.useState("");
  const requestKey = React.useRef("");
  const [busy, setBusy] = React.useState(false);
  const [token, setToken] = React.useState("");
  const [username, setUsername] = React.useState("");
  const [shaInput, setSHAInput] = React.useState("");
  const [shaFilter, setSHAFilter] = React.useState("");
  const [from, setFrom] = React.useState("");
  const [to, setTo] = React.useState("");
  const base = `/projects/${encodeURIComponent(project.id)}/history`;

  const refresh = React.useCallback(async () => {
    const next = await http<Status>(base);
    setStatus(next);
    if (next.state === "disabled" || next.indexed === 0) { setCommits([]); setListError(""); return; }
    const filters = `${shaFilter ? `&sha=${encodeURIComponent(shaFilter)}` : ""}${from ? `&from=${encodeURIComponent(from)}` : ""}${to ? `&to=${encodeURIComponent(to)}` : ""}`;
    try {
      const result = await http<{ items: Commit[] }>(`${base}/commits?candidates=${shaFilter ? 0 : 1}&offset=${offset}&limit=20${filters}`);
      setCommits(result.items); setListError("");
    } catch (error) { setCommits([]); setListError((error as Error).message); }
  }, [base, offset, shaFilter, from, to]);

  React.useEffect(() => { void refresh().catch((error) => toast.error((error as Error).message)); }, [refresh]);
  React.useEffect(() => {
    const timer = window.setInterval(() => void refresh().catch(() => {}), 10000);
    return () => window.clearInterval(timer);
  }, [refresh]);
  React.useEffect(() => {
    if (!selected || !analyses.some((item) => item.state === "pending" || item.state === "running")) return;
    const timer = window.setInterval(() => {
      void http<{ items: Analysis[] }>(`${base}/commits/${selected.commit_sha}/analyses`).then((result) => {
        setAnalyses(result.items);
        if (result.items.every((item) => item.state !== "pending" && item.state !== "running")) void refresh();
      }).catch(() => {});
    }, 3000);
    return () => window.clearInterval(timer);
  }, [analyses, base, refresh, selected]);

  async function sync() {
    setBusy(true);
    try {
      await http(base + "/sync", { method: "POST", body: JSON.stringify({ username, token }) });
      setToken("");
      await refresh();
    } catch (error) { toast.error((error as Error).message); }
    finally { setBusy(false); }
  }

  async function open(commit: Commit, path?: string) {
    setSelected(commit); setDiff(""); setAnalyses([]);
    try {
      const [source, saved] = await Promise.all([
        http<{ diff: string; truncated: boolean }>(`${base}/commits/${commit.commit_sha}/diff${path ? `?path=${encodeURIComponent(path)}` : ""}`),
        http<{ items: Analysis[] }>(`${base}/commits/${commit.commit_sha}/analyses`),
      ]);
      setDiff(source.diff); setDiffTruncated(source.truncated); setAnalyses(saved.items);
    } catch (error) { toast.error((error as Error).message); }
  }

  async function requestAnalysis() {
    if (!selected || question.trim().length < 5) return;
    setBusy(true);
    try {
      if (!requestKey.current) requestKey.current = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`;
      await http(`${base}/commits/${selected.commit_sha}/analyses`, { method: "POST", body: JSON.stringify({ question: question.trim(), idempotency_key: requestKey.current }) });
      requestKey.current = "";
      setQuestion("");
      await open(selected);
      toast.success("深入分析已加入独立作业队列");
    } catch (error) { toast.error((error as Error).message); }
    finally { setBusy(false); }
  }

  return <section className="rounded-lg border bg-card p-4" aria-label="历史修复分析">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-start gap-2">
        <GitCommitHorizontalIcon className="mt-0.5 size-4 text-primary" />
        <div><h3 className="text-sm font-semibold">历史修复分析</h3>
          <p className="text-xs text-muted-foreground">提交目录持续保存；候选只是源码线索，需结合目标版本验证。</p></div>
      </div>
      {status?.state === "running" && <span className="flex items-center gap-1 text-xs text-muted-foreground"><Loader2Icon className="size-3.5 animate-spin" />自动同步中</span>}
    </div>
    {status?.state === "disabled" && <p className="mt-3 border-t pt-3 text-xs text-muted-foreground">已加入自动历史索引队列，稍后会显示进度。</p>}
    {status?.state !== "disabled" && <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1 border-t pt-3 font-mono text-xs text-muted-foreground">
      <span>索引 {status?.indexed ?? 0}</span><span>候选 {status?.candidates ?? 0}</span><span>待处理 {status?.pending ?? 0}</span>
      <span>状态 {status?.state}</span>{status?.shallow && <span>更早历史会自动继续回溯</span>}
      {status?.rewritten && <span>远端分支历史已改写，旧记录仍保留</span>}
    </div>}
    {status?.last_error && !status.last_error.includes("历史尚未拉取到更早提交") && <p role="alert" className="mt-2 text-xs text-destructive">{status.last_error}</p>}
    {listError && <p role="alert" className="mt-2 text-xs text-muted-foreground">{listError}</p>}
    {status?.state === "interrupted" && !status.last_error.includes("历史尚未拉取到更早提交") && <details className="mt-2 text-xs"><summary className="cursor-pointer">同步失败：重试或提供私有仓库令牌</summary>
      <Input autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} placeholder="用户名（可选）" className="mt-2" />
      <Input type="password" autoComplete="new-password" value={token} onChange={(event) => setToken(event.target.value)} placeholder="令牌仅用于本次同步" className="mt-2" />
      <Button type="button" size="sm" variant="outline" disabled={busy} className="mt-2" onClick={() => void sync()}>重试同步</Button>
    </details>}
    {status?.state !== "disabled" && <div className="mt-3 grid gap-2 border-t pt-3 sm:grid-cols-[1fr_auto_auto_auto]">
      <Input aria-label="按完整提交 SHA 定位" placeholder="完整提交 SHA（留空查看候选）" value={shaInput} onChange={(event) => setSHAInput(event.target.value.trim())} className="font-mono text-xs" />
      <Input type="date" aria-label="起始日期" value={from} onChange={(event) => { setFrom(event.target.value); setOffset(0); }} className="text-xs" />
      <Input type="date" aria-label="结束日期" value={to} onChange={(event) => { setTo(event.target.value); setOffset(0); }} className="text-xs" />
      <Button type="button" variant="secondary" size="sm" disabled={shaInput !== "" && !/^[a-f0-9]{40,64}$/i.test(shaInput)} onClick={() => { setSHAFilter(shaInput.toLowerCase()); setOffset(0); }}>检索</Button>
    </div>}
    {commits.length > 0 && <div className="mt-3 divide-y border-t">
      {commits.map((commit) => <button type="button" key={commit.commit_sha} onClick={() => void open(commit)}
        className="flex w-full items-start gap-3 py-2 text-left text-xs transition-colors hover:bg-muted/40 focus-visible:outline-primary">
        <span className="shrink-0 font-mono text-primary">{commit.commit_sha.slice(0, 9)}</span>
        <span className="min-w-0 flex-1"><span className="block truncate font-medium">{commit.subject}</span>
          <span className="text-muted-foreground">{commit.reasons.join(" · ") || "人工指定"} · {commit.depth === "deep" ? "已深查" : "待深查"}</span></span>
        <span className="font-mono text-muted-foreground">{commit.score}</span>
        {commit.orphaned && <span className="text-muted-foreground">已脱离当前分支</span>}
      </button>)}
      <div className="flex items-center justify-end gap-2 pt-3 text-xs">
        <Button size="sm" variant="ghost" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - 20))}>上一页</Button>
        <span className="font-mono text-muted-foreground">{Math.floor(offset / 20) + 1}</span>
        <Button size="sm" variant="ghost" disabled={commits.length < 20} onClick={() => setOffset(offset + 20)}>下一页</Button>
      </div>
    </div>}
    {selected && <div className="mt-4 border-t pt-3">
      <p className="break-all font-mono text-xs text-primary">{selected.commit_sha}</p>
      <p className="mt-1 text-sm font-medium">{selected.subject}</p>
      <p className="mt-1 text-xs text-muted-foreground">{selected.files.slice(0, 8).join(" · ")}</p>
      {selected.files.length > 0 && <div className="mt-2 flex flex-wrap gap-1">{selected.files.slice(0, 20).map((file) => <Button key={file} type="button" size="sm" variant="ghost" className="h-auto max-w-full truncate px-2 py-1 font-mono text-[11px]" onClick={() => void open(selected, file)}>{file}</Button>)}</div>}
      <pre className="mt-3 max-h-80 overflow-auto rounded-md border bg-muted/30 p-3 text-[11px] leading-relaxed">{diff || "正在读取差异…"}</pre>
      {diffTruncated && <p className="mt-1 text-xs text-muted-foreground">差异已截取前 64 KiB，请按文件进一步检索。</p>}
      <div className="mt-3 space-y-2">{analyses.map((item) => <div key={item.id} className="rounded-md border p-2 text-xs">
        <p className="font-medium">{item.question}</p>
        <p className="mt-1 text-muted-foreground">{item.conclusion || (item.state === "failed" ? item.last_error || "分析失败，可重试" : "正在分析，结果会自动更新")}</p>
      </div>)}</div>
      <div className="mt-3 flex gap-2"><Input value={question} onChange={(event) => setQuestion(event.target.value)}
        placeholder="继续深查：例如检查同一鉴权函数的其他调用者" aria-label="深入分析问题" />
        <Button type="button" variant="secondary" disabled={busy || question.trim().length < 5} onClick={() => void requestAnalysis()}>启动深入分析</Button></div>
    </div>}
  </section>;
}
