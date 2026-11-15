"use client";

import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

// Tailwind-styled element overrides (no typography plugin in this project, so we
// style each element). `node` is stripped — it's not a valid DOM attribute.
const components: Components = {
  h1: ({ node, ...p }) => <div className="mt-2.5 font-semibold text-base" {...p} />,
  h2: ({ node, ...p }) => <div className="mt-2 font-semibold text-sm" {...p} />,
  h3: ({ node, ...p }) => <div className="mt-1.5 font-medium text-foreground/90 text-sm" {...p} />,
  h4: ({ node, ...p }) => <div className="mt-1.5 font-medium text-foreground/90 text-sm" {...p} />,
  p: ({ node, ...p }) => <p className="leading-relaxed" {...p} />,
  ul: ({ node, ...p }) => <ul className="my-1 list-disc space-y-0.5 pl-5" {...p} />,
  ol: ({ node, ...p }) => <ol className="my-1 list-decimal space-y-0.5 pl-5" {...p} />,
  strong: ({ node, ...p }) => <strong className="font-semibold" {...p} />,
  a: ({ node, ...p }) => (
    <a className="text-primary underline underline-offset-2" target="_blank" rel="noreferrer" {...p} />
  ),
  hr: () => <hr className="my-2 border-border" />,
  blockquote: ({ node, ...p }) => (
    <blockquote className="my-1 border-border border-l-2 pl-2.5 text-muted-foreground" {...p} />
  ),
  pre: ({ node, ...p }) => (
    <pre className="my-1.5 overflow-auto rounded-md bg-muted/70 p-2.5 text-xs leading-relaxed" {...p} />
  ),
  code: ({ node, className, children, ...rest }) => {
    const block = /language-/.test(className || "");
    return block ? (
      <code className={`font-mono text-xs${className || ""}`} {...rest}>
        {children}
      </code>
    ) : (
      <code className="break-all rounded bg-muted px-1 py-0.5 font-mono text-[0.85em]" {...rest}>
        {children}
      </code>
    );
  },
  table: ({ node, ...p }) => (
    <div className="my-1.5 overflow-x-auto">
      <table className="w-full border-collapse text-xs" {...p} />
    </div>
  ),
  th: ({ node, ...p }) => (
    <th className="border border-border bg-muted/60 px-2 py-1 text-left font-medium" {...p} />
  ),
  td: ({ node, ...p }) => <td className="border border-border px-2 py-1 align-top" {...p} />,
};

// artifact:<id> 图片解析（与 docx 导出 writeMarkdownish 同一约定）：reporter 把
// `![说明](artifact:<id>)` 写进详细报告，详情页/报告页在原位内嵌截图。taskId 缺省
// 时（如技能预览）降级为占位文本，不渲染破图。
function imgOverride(taskId?: string) {
  return ({ alt, src, ...p }: React.ImgHTMLAttributes<HTMLImageElement> & { node?: unknown }) => {
    const s = typeof src === "string" ? src : "";
    const m = /^artifact:(\d+)$/.exec(s);
    if (m) {
      if (!taskId) {
        return <span className="text-muted-foreground text-xs">[现场截图 artifact:{m[1]}，报告导出时内嵌]</span>;
      }
      return (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={`/api/tasks/${taskId}/artifacts/${m[1]}`}
          alt={alt || `artifact:${m[1]}`}
          loading="lazy"
          className="my-1.5 max-h-[70vh] max-w-full rounded-md border border-border"
          {...p}
        />
      );
    }
    // eslint-disable-next-line @next/next/no-img-element
    return <img src={s} alt={alt ?? ""} loading="lazy" className="my-1.5 max-w-full rounded-md border border-border" {...p} />;
  };
}

export function Markdown({ text, taskId }: { text: string; taskId?: string }) {
  const overrides: Components = { ...components, img: imgOverride(taskId) };
  return (
    <div className="min-w-0 space-y-1.5 text-sm [&>*:first-child]:mt-0">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {text}
      </ReactMarkdown>
    </div>
  );
}
