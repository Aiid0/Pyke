"use client";

import * as React from "react";
import { GitBranchIcon, GitCommitHorizontalIcon, Loader2Icon } from "lucide-react";
import { toast } from "sonner";

import { ProjectHistoryPanel } from "@/components/project-history-panel";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { api } from "@/lib/api";
import type { ImportedProject } from "@/lib/types";

export default function HistoryPage() {
  const [projects, setProjects] = React.useState<ImportedProject[]>([]);
  const [selectedID, setSelectedID] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [importing, setImporting] = React.useState(false);
  const [name, setName] = React.useState("");
  const [repositoryURL, setRepositoryURL] = React.useState("");
  const [branch, setBranch] = React.useState("");
  const [username, setUsername] = React.useState("");
  const [token, setToken] = React.useState("");

  React.useEffect(() => {
    api.projects().then((items) => {
      const git = items.filter((item) => item.source_type === "git");
      setProjects(git);
      setSelectedID((current) => current || git[0]?.id || "");
    }).catch((error) => toast.error((error as Error).message)).finally(() => setLoading(false));
  }, []);

  async function importRepository(event: React.FormEvent) {
    event.preventDefault();
    if (!repositoryURL.trim() || importing) return;
    setImporting(true);
    try {
      const project = await api.importGitProject({ name: name.trim(), repository_url: repositoryURL.trim(), branch: branch.trim(), username, token });
      setProjects((previous) => [project, ...previous.filter((item) => item.id !== project.id)]);
      setSelectedID(project.id);
      setToken("");
      toast.success("仓库已导入，可以开始历史索引");
    } catch (error) { toast.error((error as Error).message); }
    finally { setImporting(false); }
  }

  const selected = projects.find((item) => item.id === selectedID);

  return <div className="mx-auto flex w-full max-w-6xl flex-col gap-5 p-4 md:p-6">
    <div className="flex items-start gap-3">
      <div className="rounded-xl border bg-primary/10 p-2.5 text-primary"><GitCommitHorizontalIcon className="size-5" /></div>
      <div><h1 className="text-xl font-semibold">历史修复分析</h1>
        <p className="mt-1 text-sm text-muted-foreground">从提交与修复记录寻找值得复核的安全线索，逐步保存索引和分析结果。</p></div>
    </div>

    <div className="grid gap-5 lg:grid-cols-[minmax(250px,0.7fr)_minmax(0,1.7fr)]">
      <div className="space-y-5">
        <Card><CardHeader><CardTitle className="text-sm">选择仓库</CardTitle></CardHeader><CardContent>
          <NativeSelect aria-label="选择历史分析仓库" value={selectedID} onChange={(event) => setSelectedID(event.target.value)} disabled={loading} className="w-full">
            <NativeSelectOption value="">{loading ? "正在加载仓库…" : "选择 Git 项目"}</NativeSelectOption>
            {projects.map((project) => <NativeSelectOption key={project.id} value={project.id}>{project.name}{project.branch ? ` · ${project.branch}` : ""}</NativeSelectOption>)}
          </NativeSelect>
          {!loading && projects.length === 0 && <p className="mt-2 text-xs text-muted-foreground">暂无 Git 项目，可在下方导入仓库。</p>}
        </CardContent></Card>
        <Card><CardHeader><CardTitle className="flex items-center gap-2 text-sm"><GitBranchIcon className="size-4" />导入 Git 仓库</CardTitle></CardHeader>
          <CardContent><form onSubmit={(event) => void importRepository(event)} className="grid gap-2.5">
            <Input aria-label="项目名称" placeholder="项目名称（可选）" maxLength={200} value={name} onChange={(event) => setName(event.target.value)} disabled={importing} />
            <Input aria-label="Git 仓库地址" type="url" required placeholder="https://host/owner/repo.git" value={repositoryURL} onChange={(event) => setRepositoryURL(event.target.value)} disabled={importing} />
            <Input aria-label="Git 分支" placeholder="分支（可选）" value={branch} onChange={(event) => setBranch(event.target.value)} disabled={importing} />
            <details className="text-xs text-muted-foreground"><summary className="cursor-pointer">私有仓库认证</summary><div className="mt-2 grid gap-2">
              <Input aria-label="Git 用户名" placeholder="用户名" value={username} onChange={(event) => setUsername(event.target.value)} disabled={importing} />
              <Input aria-label="Git 访问令牌" type="password" autoComplete="new-password" placeholder="本次导入使用的令牌" value={token} onChange={(event) => setToken(event.target.value)} disabled={importing} />
            </div></details>
            <Button type="submit" disabled={importing || !repositoryURL.trim()}>{importing && <Loader2Icon className="size-4 animate-spin" />}{importing ? "正在导入…" : "导入仓库"}</Button>
          </form></CardContent></Card>
      </div>
      <div>{selected ? <ProjectHistoryPanel key={selected.id} project={selected} /> : <Card><CardContent className="py-16 text-center text-sm text-muted-foreground">选择或导入仓库后开始历史分析</CardContent></Card>}</div>
    </div>
  </div>;
}
