"use client";
import * as React from "react";

import { FlaskConicalIcon, RefreshCwIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { FuzzEnvironment as Environment } from "@/lib/fuzz-types";

export function FuzzEnvironment() {
  const [env, setEnv] = React.useState<Environment | null>(null);
  const [error, setError] = React.useState("");
  const refresh = React.useCallback(() => {
    setError("");
    api
      .fuzzEnvironment()
      .then(setEnv)
      .catch((e: Error) => setError(e.message));
  }, []);
  React.useEffect(refresh, [refresh]);
  return (
    <div className="flex items-start gap-3 rounded-lg border bg-card p-4">
      <FlaskConicalIcon className="mt-0.5 size-5 shrink-0 text-primary" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-sm">LibAFL 执行环境</p>
        <p role="status" className="mt-1 break-words text-muted-foreground text-xs">
          {error ||
            (env ? (env.available ? "执行镜像已就绪 · 工作区映射将在启动前验证" : env.reason) : "检查执行镜像…")}
        </p>
      </div>
      <Button size="icon" variant="ghost" aria-label="刷新 LibAFL 环境" onClick={refresh}>
        <RefreshCwIcon className="size-4" />
      </Button>
    </div>
  );
}
