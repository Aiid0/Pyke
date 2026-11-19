# VM Lab 攻击效果验证 MCP — 设计文档

> 状态：已实施（P0）。需求来源：RCE 挖到之后「验证攻击效果」是证据链的关键环节，
> 用户已部署 VirtualBox 靶机。本文是设计记录 + 运维配置手册。

## 1. 需求与动机

- 挖到漏洞（RCE/提权/上传）后，平台闸门要求 critical/high 有 L3+ 证据；现状只有
  「回连 shell 人工接管 + 证据纪律」两条路，缺「干净靶场 + 自动取证 + 状态还原」闭环。
- 「模拟点击」分层解决：**Web 应用点击**走既有 Playwright MCP（browser 预设）直接访问
  靶机网段，不需要 guest 内动作；**VM 桌面点击**（二期）用 vm_exec 在 guest 内跑
  自动化脚本（pyautogui / PowerShell SendKeys）——VBoxManage 无鼠标注入 API，
  不引入 vboxapi/vboxwebsrv 重依赖。

## 2. 架构

```
worker/planner（平台内 agent）
  └─ deferred tools: mcp__vm-lab__vm_exec 等
       └─ ExecuteExtraTool → guard 解包（v0.4.71 新增）→ 三层拦截 deny/judge/ask
            └─ norma MCP stdio client → mcp-servers/vm-lab/（FastMCP，uv 拉起）
                 └─ 驱动层（VM_DRIVER，可插拔）→ VBoxManage CLI
                      ├─ local       本机 VBoxManage
                      ├─ win-interop Windows 宿主 exe（WSL2 interop 直调）
                      └─ ssh         远程 Linux VBox 宿主
```

工具集（10 个）：vm_list / vm_start / vm_stop / vm_snapshot_list / vm_snapshot_take /
vm_snapshot_restore / vm_exec / vm_screenshot / vm_copyto / vm_copyfrom。

安全护栏（MCP 内部强制）：VBoxManage argv 按白名单形态构造（不接受自由命令串）；
VM 名/快照名正则 + 存在性校验；restore / poweroff 需 confirm=true；guest 凭据只进
MCP 环境变量（`--passwordfile` 临时文件 0600 传递）；per-VM 串行锁。

## 3. 平台级安全增强：MCP 调用纳入拦截（v0.4.71）

**缺口**（实施前实测确认）：模型调 MCP 工具走 norma `ExecuteExtraTool` 包装，
guard 的 PreToolUse 只看到外层名 `ExecuteExtraTool`，而它不在 `defaultEnabledTools`
里 → **MCP 调用完全绕过 deny/judge/ask 三层闸门**。vm_exec 这类能力裸奔不可接受。

**修复**（guard/guard.go + intercept/intercept.go）：
1. guard 解包：外层名 == ExecuteExtraTool 时，取内层 `{tool_name, params}` 作为
   拦截与审计对象（工具名 = `mcp__vm-lab__vm_exec`；params 摊平进审计 command 面）；
2. intercept：`defaultEnabledTools` 增加 `mcp__*` 通配记号；`IsToolEnabled` 对
   `mcp__` 前缀的工具有该记号即放行进规则匹配。**未存过自定义 enabled 列表的部署
   默认生效**；存过自定义列表的部署不受影响（想纳入就在列表里加 `mcp__*`）；
3. 行为影响：只读类 MCP（FOFA 查询等）无规则命中即放行，行为不变；judge 开启时
   未命中规则的 MCP 调用会进 LLM 兜底研判（三层闸门的本意）；guard 审计里 MCP
   调用记内层工具名与参数摘要。

回归测试：`guard/mcp_unwrap_test.go`（deny 按内层名命中 / params 进 tool_input 面 /
无规则放行 / 畸形输入不 panic）。

## 4. 配置矩阵（MCP 环境变量）

| 变量 | 作用 | 默认 |
|---|---|---|
| `VM_DRIVER` | auto / local / win-interop / ssh | auto（探 win-interop → local） |
| `VM_VBOXMANAGE` | local 驱动的 VBoxManage 路径 | PATH |
| `VM_WIN_VBOXMANAGE` | win-interop 的 exe 路径 | 探 /mnt/c\|d\|e 下常见安装位 |
| `VM_SSH_HOST/PORT/USER/KEY` | ssh 驱动目标 | HOST 必填 |
| `VM_GUEST_USER/PASSWORD` | guest 全局凭据（GA） | 无（vm_exec 报错给指引） |
| `VM_GUEST_CREDENTIALS` | per-VM 凭据 JSON | 无 |
| `VM_SCREENSHOT_DIR` | 截图目录（**VBox 宿主机侧路径**） | ./vm-screenshots |

⚠️ 路径语义：win-interop/ssh 驱动下，截图目录 / copyto 源路径都是 **VBox 宿主机
侧的路径**（WSL 场景给 Windows 路径如 `C:\vm-shots`，WSL 侧经 `/mnt/c` 取产物归档）。

## 5. 验收与回退

- 单测：`go test ./guard/... ./intercept/...`；
- 冒烟：`uv --directory mcp-servers/vm-lab run python vm_lab.py` + JSON-RPC
  initialize / tools-list；无 VBox 环境时 vm_list 应返回明确的驱动探测报错文案；
- 平台内：MCP 页启用 vm-lab → agent_visibility 授权 → 会话内 SearchExtraTools →
  vm_list；guard 审计应出现内层工具名（拦截闭环验证）；
- 回退：MCP 页禁用 vm-lab 即整体下线；guard 解包是通用增强，独立可用、无开关需求。

## 5.1 RCE 视觉证据方法学（弹计算器判据）

「靶机桌面弹出计算器/执行指定无害命令」是仓库认可的 RCE 确认标准（src-hunting
参考文档同判据）。标准证据链四件套，缺一降级：

1. **远程触发**：http_request 走真实网络路径发 payload——HTTP 回显即 report_finding
   闸门要求的机械凭据（闸门信号表不认截图，`agent/finding_recorder.go`），全程 MITM 留痕；
2. **桌面视觉证据**：vm_screenshot 拍弹窗（calc/记事本/命令窗口）；
3. **状态取证**：vm_exec 查进程/文件/持久化，推到 L4；
4. **save_evidence** 绑 finding_id。

关键坑——**会话语义**：被利用服务多在 session 0/服务账号，payload 直接 `calc.exe`
弹在不可见桌面；用 `schtasks /IT`（交互标志）落到控制台会话再截图。Linux 靶机用
`DISPLAY=:0` + xmessage/gedit，无 GUI 靶机用 Web 面变化 + 文件落地替代。vm_exec
是平台 guest 通道，不能冒充远程利用路径，只做效果取证。完整教程见
`skills/vm-lab-verification/SKILL.md`（worker 技能路由已收录该场景）。

报告落位：截图引用 `![说明](artifact:<id>)` 由 reporter 写进详细报告（与 PoC 同节
并列）——docx 导出经 `writeMarkdownish` 原位内嵌；前端漏洞详情页/任务报告页/
发现列表展开行也已支持 `artifact:<id>` 内联渲染（`web/src/components/markdown.tsx`，
与 docx 同一约定）。注意引用只写 Report 字段，evidence 字段导出时固定为代码块。

## 5.2 任务资源租约与回收（v0.5.0）

VM 会持续占用 CPU、内存和网络，不能按普通的一次性 MCP 调用处理。平台将它视为
**任务托管资源**：Agent 对关机 VM 调用 `vm_start` 时，MCP 才会写入当前任务私有租约；
若 VM 已在运行，则只使用、不接管，绝不自动关闭，保护人工或其他任务启动的靶机。

| 事件 | 行为 |
| --- | --- |
| 主 Agent 点击停止 | 取消本轮对话，并异步停止该任务的 Fuzz 与托管 VM |
| 暂停/删除任务 | 同步回收托管 VM；回收失败时保留租约以供重试和审计 |
| Pyke 服务退出/重启 | 扫描遗留租约并回收，避免宿主机遗留高 CPU 虚机 |

回收先发送 ACPI 优雅关机，最多等待 30 秒；仅对有租约的 VM 才会 `poweroff`。
自动回收**不会自动还原快照**，避免销毁尚未归档的取证现场。任务上下文只通过
vm-lab 子进程的 `PYKE_VM_LEASE_FILE` 注入，不修改 MCP 数据库配置，也不暴露给模型。

## 6. 二期（未做）

- 原生 Go 工具版（agent/tools_vm.go 三清单 + BuiltinToolSeeds）：审批/证据全链路
  更顺（save_evidence 直连 runDir），适合要深度绑定时做；
- VM 桌面点击：vm_exec + pyautogui/SendKeys 脚本封装；
- 多靶场编排：一键重置全部靶机（场景模板）。
