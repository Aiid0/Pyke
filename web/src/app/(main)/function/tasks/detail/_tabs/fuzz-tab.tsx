"use client";
import * as React from "react";

import { BotIcon, FlaskConicalIcon, PlayIcon, RefreshCwIcon, SquareIcon } from "lucide-react";
import { toast } from "sonner";

import { FuzzEnvironment } from "@/components/fuzz-environment";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { type FuzzJob, type FuzzResults, fuzzActive } from "@/lib/fuzz-types";

const states: Record<string, string> = {
  queued: "排队中",
  preparing: "准备中",
  running: "运行中",
  stopping: "停止中",
  stopped: "已停止",
  completed: "运行结束",
  failed: "失败",
  interrupted: "运行中断",
};

export function FuzzTab({ taskId }: { taskId: string }) {
  const [jobs, setJobs] = React.useState<FuzzJob[]>([]);
  const [selected, setSelected] = React.useState<number | null>(null);
  const [results, setResults] = React.useState<FuzzResults | null>(null);
  const [logs, setLogs] = React.useState("");
  const [offset, setOffset] = React.useState(0);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [form, setForm] = React.useState({
    source: "",
    build: "build.sh",
    seeds: "seeds",
    dictionary: "",
    minutes: 30,
    memory_mb: 2048,
    cpus: 1,
    auto_analyze: false,
  });
  const generation = React.useRef(0);
  const requestID = React.useRef("");
  const refresh = React.useCallback(async () => {
    try {
      const data = await api.fuzzJobs(taskId);
      setJobs(data);
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, [taskId]);
  React.useEffect(() => {
    void refresh();
    const timer = setInterval(() => {
      if (!document.hidden) void refresh();
    }, 3000);
    return () => clearInterval(timer);
  }, [refresh]);
  React.useEffect(() => {
    const token = ++generation.current;
    setResults(null);
    setLogs("");
    setOffset(0);
    if (selected === null) return;
    const load = () =>
      api
        .fuzzResults(taskId, selected)
        .then((data) => {
          if (generation.current === token) setResults(data);
        })
        .catch((e: Error) => {
          if (generation.current === token) setError(e.message);
        });
    void load();
    const timer = setInterval(() => {
      if (!document.hidden) void load();
    }, 3000);
    return () => {
      generation.current++;
      clearInterval(timer);
    };
  }, [taskId, selected]);
  async function action(id: number, name: string, data: Record<string, unknown> = {}) {
    setBusy(true);
    try {
      await api.fuzzAction(taskId, id, name, { request_id: crypto.randomUUID(), ...data });
      toast.success("操作已接受");
      await refresh();
      if (selected === id) setResults(await api.fuzzResults(taskId, id));
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function start(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    try {
      if (!requestID.current) requestID.current = crypto.randomUUID();
      const j = await api.fuzzStart(taskId, { ...form, seconds: form.minutes * 60, request_id: requestID.current });
      requestID.current = "";
      setSelected(j.id);
      await refresh();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function loadLogs() {
    if (selected === null) return;
    const token = generation.current;
    try {
      const data = await api.fuzzLogs(taskId, selected, offset);
      if (generation.current !== token) return;
      setLogs((old) => (old + data.text).slice(-128000));
      setOffset(data.next_offset);
    } catch (e) {
      toast.error((e as Error).message);
    }
  }
  return (
    <div className="space-y-4">
      <FuzzEnvironment />
      <div className="grid gap-4 xl:grid-cols-[20rem_minmax(0,1fr)]">
        <form onSubmit={start} className="space-y-4 self-start rounded-lg border bg-card p-4">
          <div className="flex items-center gap-2">
            <FlaskConicalIcon className="size-4 text-primary" />
            <h3 className="font-medium">新建 Fuzz 作业</h3>
          </div>
          <p className="text-muted-foreground text-xs">持续测试本地解析入口。Agent 回合结束后作业仍会运行。</p>
          {(
            [
              ["source", "任务内源码目录"],
              ["build", "构建脚本"],
              ["seeds", "种子目录"],
              ["dictionary", "字典（可选）"],
            ] as const
          ).map(([key, label]) => (
            <div key={key} className="space-y-1.5">
              <Label htmlFor={`fuzz-${key}`}>{label}</Label>
              <Input
                id={`fuzz-${key}`}
                value={form[key]}
                required={key !== "dictionary"}
                onChange={(e) => {
                  requestID.current = "";
                  setForm({ ...form, [key]: e.target.value });
                }}
                placeholder={key === "source" ? "shared/my-parser" : ""}
              />
            </div>
          ))}
          <p className="text-muted-foreground text-xs">构建脚本、种子和字典路径相对源码目录。</p>
          <div className="grid grid-cols-3 gap-2">
            {(
              [
                ["minutes", "分钟", 1, 1440],
                ["cpus", "CPU", 1, 4],
                ["memory_mb", "内存 MiB", 256, 16384],
              ] as const
            ).map(([key, label, min, max]) => (
              <div key={key} className="space-y-1.5">
                <Label htmlFor={`fuzz-${key}`}>{label}</Label>
                <Input
                  id={`fuzz-${key}`}
                  type="number"
                  min={min}
                  max={max}
                  required
                  value={form[key]}
                  onChange={(e) => {
                    requestID.current = "";
                    setForm({ ...form, [key]: Number(e.target.value) });
                  }}
                />
              </div>
            ))}
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={form.auto_analyze}
              onChange={(e) => {
                requestID.current = "";
                setForm({ ...form, auto_analyze: e.target.checked });
              }}
            />
            自动分析新异常
          </label>
          <p className="text-muted-foreground text-xs">每次运行最多自动派发 5 个分析任务，其他样本保留待处理。</p>
          <Button type="submit" className="w-full" disabled={busy}>
            <PlayIcon className="size-4" />
            启动 LibAFL
          </Button>
        </form>
        <div className="min-w-0 space-y-4">
          <div className="rounded-lg border bg-card">
            <div className="flex items-center justify-between border-b px-4 py-3">
              <h3 className="font-medium text-sm">
                作业记录 <span className="ml-2 text-muted-foreground">{jobs.length}</span>
              </h3>
              <Button size="icon" variant="ghost" aria-label="刷新作业" onClick={() => void refresh()}>
                <RefreshCwIcon className="size-4" />
              </Button>
            </div>
            {error && (
              <p role="alert" className="p-4 text-destructive text-sm">
                {error}
              </p>
            )}
            {!jobs.length && !error && (
              <div className="px-4 py-10 text-center text-muted-foreground text-sm">
                尚无 Fuzz 作业。手动或通过 Worker 启动后，会在这里统一显示。
              </div>
            )}
            <div className="max-h-72 overflow-auto">
              {jobs.map((j) => (
                <button
                  type="button"
                  key={j.id}
                  onClick={() => setSelected(j.id)}
                  className={`flex w-full items-center gap-3 border-b px-4 py-3 text-left last:border-0 hover:bg-muted/40 ${selected === j.id ? "bg-muted/60" : ""}`}
                >
                  <span className="font-mono text-xs">#{j.id}</span>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm">{j.config.source}</p>
                    <p className="text-muted-foreground text-xs">
                      {j.creator} · {new Date(j.created_at).toLocaleString()}
                    </p>
                  </div>
                  <Badge variant="outline">{states[j.state] ?? j.state}</Badge>
                </button>
              ))}
            </div>
          </div>
          {results && (
            <div className="space-y-4 rounded-lg border bg-card p-4">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="mr-auto font-medium">作业 #{results.job.id}</h3>
                {fuzzActive(results.job.state) ? (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void action(results.job.id, "stop")}
                  >
                    <SquareIcon className="size-3" />
                    停止
                  </Button>
                ) : (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void action(results.job.id, "continue")}
                  >
                    <PlayIcon className="size-3" />
                    继续运行
                  </Button>
                )}
              </div>
              {results.job.reason && <p className="break-words text-muted-foreground text-xs">{results.job.reason}</p>}
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                {[
                  ["执行速率", results.job.metrics.execs_per_sec ?? "—"],
                  ["覆盖反馈计数", results.job.metrics.coverage ?? "—"],
                  ["语料数", results.job.metrics.corpus_count ?? "—"],
                  ["异常组", results.groups.length],
                ].map(([label, value]) => (
                  <div key={label} className="rounded-md bg-muted/40 p-3">
                    <p className="text-muted-foreground text-xs">{label}</p>
                    <p className="mt-1 font-mono text-lg">{value}</p>
                  </div>
                ))}
              </div>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  disabled={busy}
                  checked={results.job.config.auto_analyze}
                  onChange={(e) => void action(results.job.id, "auto", { auto_analyze: e.target.checked })}
                />
                自动交给 AI 分析
              </label>
              <p className="text-muted-foreground text-xs">
                异常样本尚不代表已确认漏洞。证据目录：<code>{results.workspace}</code>
              </p>
              {results.groups.map((g) => (
                <div key={g.id} className="space-y-2 rounded-md border p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="secondary">{g.kind}</Badge>
                    <span className="font-mono text-xs">
                      #{g.id} · {g.count} 个样本
                    </span>
                    <span className="ml-auto text-muted-foreground text-xs">
                      {g.intent_id ? `分析意图 #${g.intent_id} · ${g.analysis_state}` : "待分析"}
                    </span>
                  </div>
                  <div className="flex gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={busy}
                      onClick={() => void action(results.job.id, "replay", { group_id: g.id })}
                    >
                      回放样本
                    </Button>
                    <Button
                      size="sm"
                      disabled={busy || !!g.intent_id}
                      onClick={() => void action(results.job.id, "analyze", { group_id: g.id })}
                    >
                      <BotIcon className="size-4" />
                      交给 AI 分析
                    </Button>
                  </div>
                  <details>
                    <summary className="cursor-pointer text-muted-foreground text-xs">异常输出与证据</summary>
                    <p className="mt-2 break-all font-mono text-xs">
                      {results.workspace}/samples/{g.sample}
                    </p>
                    <pre className="mt-2 max-h-52 overflow-auto whitespace-pre-wrap text-xs">{g.detail}</pre>
                  </details>
                </div>
              ))}
              <div>
                <Button size="sm" variant="ghost" onClick={() => void loadLogs()}>
                  读取更多日志
                </Button>
                <pre className="mt-2 max-h-72 overflow-auto rounded-md bg-muted/40 p-3 font-mono text-xs">
                  {logs || "点击读取日志，每次最多 64 KiB。"}
                </pre>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
