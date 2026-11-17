import type { FuzzConfig, FuzzJob } from "@/lib/fuzz-types";

const jobs: FuzzJob[] = [];
export function mockFuzz(method: string, path: string, body: Record<string, unknown>): unknown {
  if (path === "/fuzz/environment") return { available: true, reason: "", image: "mock-libafl" };
  const match = /^\/tasks\/([^/]+)\/fuzz-jobs(?:\/(\d+)\/(\w+))?$/.exec(path);
  if (!match) return undefined;
  const [, task, id, action] = match;
  if (!id && method === "GET") return jobs.filter((j) => j.task_id === task);
  if (!id) {
    const previous = jobs.find((j) => j.task_id === task && j.config.request_id === body.request_id);
    if (previous) return previous;
    const job: FuzzJob = {
      id: jobs.length + 1,
      task_id: task,
      creator: "human",
      state: "completed",
      reason: "模拟运行结束",
      config: body as unknown as FuzzConfig,
      image: "mock-libafl",
      snapshot: "mock",
      metrics: { corpus_count: 12, coverage: 48, execs_per_sec: 1200 },
      created_at: new Date().toISOString(),
    };
    jobs.unshift(job);
    return job;
  }
  const job = jobs.find((j) => j.task_id === task && j.id === Number(id));
  if (!job) throw new Error("作业不存在");
  if (action === "results") return { job, groups: [], workspace: `shared/fuzz/${id}` };
  if (action === "logs") return { text: "[mock] LibAFL campaign finished.\n", next_offset: 0 };
  if (action === "auto") job.config.auto_analyze = Boolean(body.auto_analyze);
  if (action === "stop") job.state = "stopped";
  if (action === "continue")
    return mockFuzz("POST", `/tasks/${task}/fuzz-jobs`, { ...job.config, request_id: body.request_id });
  return job;
}
