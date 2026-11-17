"use client";

import * as React from "react";
import { CheckCircle2Icon, FolderOpenIcon, GitBranchIcon, UploadIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import type { ImportedProject } from "@/lib/types";

type SourceMode = "none" | "existing" | "git" | "zip" | "directory";

export function ProjectSourcePicker({ value, onChange, onBusyChange, onRequiredChange }: {
  value: ImportedProject | null;
  onChange: (value: ImportedProject | null) => void;
  onBusyChange: (busy: boolean) => void;
  onRequiredChange: (required: boolean) => void;
}) {
  const [mode, setMode] = React.useState<SourceMode>(value ? "existing" : "none");
  const [projects, setProjects] = React.useState<ImportedProject[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState(false);
  const [name, setName] = React.useState("");
  const [remote, setRemote] = React.useState("");
  const [branch, setBranch] = React.useState("");
  const [username, setUsername] = React.useState("");
  const [token, setToken] = React.useState("");
  const zipRef = React.useRef<HTMLInputElement>(null);
  const folderRef = React.useRef<HTMLInputElement>(null);
  const mounted = React.useRef(true);

  React.useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; onBusyChange(false); };
  }, [onBusyChange]);

  React.useEffect(() => {
    api.projects().then(setProjects).catch(() => toast.error("加载已导入项目失败，可重新导入项目")).finally(() => setLoading(false));
  }, []);

  React.useEffect(() => { onRequiredChange(mode !== "none"); }, [mode, onRequiredChange]);

  const changeMode = (next: SourceMode) => {
    setMode(next);
    onChange(null);
    onRequiredChange(next !== "none");
  };
  const setImporting = (next: boolean) => { if (mounted.current) { setBusy(next); onBusyChange(next); } };
  const accept = (project: ImportedProject) => {
    if (!mounted.current) return;
    setProjects((previous) => [project, ...previous.filter((item) => item.id !== project.id)]);
    onChange(project);
    setToken("");
    toast.success(`项目「${project.name}」已导入`);
  };
  async function clone() {
    if (!remote.trim() || busy) return;
    setImporting(true);
    try { accept(await api.importGitProject({name: name.trim(), repository_url: remote.trim(), branch: branch.trim(), username, token})); }
    catch (error) { if (mounted.current) toast.error((error as Error).message); }
    finally { setImporting(false); if (mounted.current) setToken(""); }
  }
  async function upload(files: File[], source: "zip" | "directory") {
    if (!files.length || busy) return;
    setImporting(true);
    try { accept(await api.uploadProject(files, source, name.trim())); }
    catch (error) { if (mounted.current) toast.error((error as Error).message); }
    finally { setImporting(false); }
  }

  return (
    <Field className="gap-3 rounded-lg border bg-muted/20 p-4">
      <div className="flex items-center gap-2">
        <FolderOpenIcon className="size-4 text-primary" />
        <FieldLabel htmlFor="project-source-mode">测试项目</FieldLabel>
      </div>
      <NativeSelect id="project-source-mode" className="w-full" value={mode} disabled={busy}
        onChange={(event) => changeMode(event.target.value as SourceMode)}>
        <NativeSelectOption value="none">网站 / IP 等目标（在描述中填写）</NativeSelectOption>
        <NativeSelectOption value="existing">选择已导入项目</NativeSelectOption>
        <NativeSelectOption value="git">Git / Gitee 仓库</NativeSelectOption>
        <NativeSelectOption value="zip">上传项目 ZIP</NativeSelectOption>
        <NativeSelectOption value="directory">上传本地项目文件夹</NativeSelectOption>
      </NativeSelect>
      {mode === "none" ? (
        <FieldDescription>源码审计请选择仓库或本地项目；网站测试可在描述中填写 URL、IP 与授权范围。</FieldDescription>
      ) : mode === "existing" ? (
        <div className="grid gap-2">
          <NativeSelect id="project-existing" aria-label="选择已导入项目" className="w-full" disabled={loading}
            value={value?.id ?? ""} onChange={(event) => onChange(projects.find((p) => p.id === event.target.value) ?? null)}>
            <NativeSelectOption value="">{loading ? "正在加载项目…" : "选择一个项目"}</NativeSelectOption>
            {projects.map((p) => <NativeSelectOption key={p.id} value={p.id}>{p.name}{p.branch ? ` · ${p.branch}` : ""}</NativeSelectOption>)}
          </NativeSelect>
          {!loading && projects.length === 0 && <FieldDescription>暂无项目，可切换到仓库导入或本地上传。</FieldDescription>}
        </div>
      ) : (
        <div className="grid gap-3">
          <Input id="project-import-name" aria-label="项目名称" value={name} maxLength={200}
            placeholder="项目名称（可选，默认使用仓库或文件夹名称）" disabled={busy} onChange={(event) => setName(event.target.value)} />
          {mode === "git" ? <>
            <Input id="project-repository-url" aria-label="Git 仓库地址" type="url" value={remote} disabled={busy}
              placeholder="https://gitee.com/owner/project.git" onChange={(event) => setRemote(event.target.value)} />
            <Input id="project-branch" aria-label="仓库分支" value={branch} disabled={busy}
              placeholder="分支（可选，留空使用仓库默认分支）" onChange={(event) => setBranch(event.target.value)} />
            <details className="text-sm">
              <summary className="cursor-pointer text-muted-foreground">私有仓库认证（可选）</summary>
              <div className="mt-2 grid gap-2">
                <Input aria-label="仓库用户名" autoComplete="off" value={username} disabled={busy} placeholder="Git 服务要求的用户名"
                  onChange={(event) => setUsername(event.target.value)} />
                <Input aria-label="仓库访问令牌" type="password" autoComplete="new-password" value={token} disabled={busy} placeholder="访问令牌，仅用于本次导入"
                  onChange={(event) => setToken(event.target.value)} />
              </div>
            </details>
            <Button type="button" variant="outline" disabled={busy || !remote.trim()} onClick={() => void clone()}>
              {busy ? <Spinner /> : <GitBranchIcon />}{busy ? "正在克隆仓库…" : "导入仓库"}
            </Button>
          </> : <>
            <input ref={zipRef} type="file" accept=".zip" className="hidden" disabled={busy}
              onChange={(event) => { void upload(Array.from(event.target.files ?? []), "zip"); event.target.value = ""; }} />
            <input ref={folderRef} type="file" multiple className="hidden" disabled={busy}
              {...({webkitdirectory: "", directory: ""} as React.InputHTMLAttributes<HTMLInputElement>)}
              onChange={(event) => { void upload(Array.from(event.target.files ?? []), "directory"); event.target.value = ""; }} />
            <Button type="button" variant="outline" disabled={busy} onClick={() => (mode === "zip" ? zipRef : folderRef).current?.click()}>
              {busy ? <Spinner /> : <UploadIcon />}{busy ? "正在上传项目…" : mode === "zip" ? "选择项目 ZIP" : "选择本地项目文件夹"}
            </Button>
            <FieldDescription>保留项目目录结构；上传总量最多 512 MiB，导入后最多 1 GiB、30000 个文件。</FieldDescription>
          </>}
        </div>
      )}
      {value && <div role="status" className="grid gap-1 rounded-md border bg-background p-3 text-sm">
        <div className="flex items-center gap-2 font-medium"><CheckCircle2Icon className="size-4 text-primary" />{value.name}</div>
        <p className="text-muted-foreground text-xs">{value.file_count} 个文件{value.branch ? ` · ${value.branch}` : ""}{value.revision ? ` · ${value.revision.slice(0, 12)}` : ""}</p>
        <p className="text-muted-foreground text-xs">创建任务时生成独立源码快照，后续会话持续使用该项目。</p>
      </div>}
    </Field>
  );
}
