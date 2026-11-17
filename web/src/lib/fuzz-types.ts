export interface FuzzConfig {
  source: string;
  build: string;
  seeds: string;
  dictionary?: string;
  request_id: string;
  seconds?: number;
  build_seconds?: number;
  timeout?: number;
  memory_mb?: number;
  cpus?: number;
  max_input?: number;
  output_mb?: number;
  auto_analyze: boolean;
  parent_id?: number;
}
export interface FuzzJob {
  id: number;
  task_id: string;
  creator: string;
  state: string;
  reason: string;
  config: FuzzConfig;
  image: string;
  snapshot: string;
  created_at: string;
  metrics: Record<string, number | string>;
}
export interface FuzzGroup {
  id: number;
  job_id: number;
  kind: string;
  sample: string;
  detail: string;
  count: number;
  intent_id: number;
  analysis_state: string;
}
export interface FuzzResults {
  groups: FuzzGroup[];
  workspace: string;
  job: FuzzJob;
}
export interface FuzzEnvironment {
  available: boolean;
  image: string;
  reason: string;
}
export const fuzzActive = (state: string) => ["queued", "preparing", "running", "stopping"].includes(state);
