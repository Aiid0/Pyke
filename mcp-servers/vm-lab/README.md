# vm-lab — VirtualBox 靶机控制 MCP

攻击效果验证 MCP：挖到漏洞（尤其 RCE）之后，用 VirtualBox 靶机验证攻击效果并取证——快照基线 → 攻击 → guest 内取证（vm_exec / vm_screenshot / vm_copyfrom）→ 报告 → 快照还原。

## 工具清单

| 工具 | 作用 | 破坏性 |
|---|---|---|
| `vm_list` | 靶场清单（名称/状态/快照/内存/CPU） | 只读 |
| `vm_start` | 启动 VM（默认 headless） | 低 |
| `vm_stop` | 关机（acpi 温和 / poweroff 硬断电需 confirm） | 中 |
| `vm_snapshot_list` | 快照树与当前快照 | 只读 |
| `vm_snapshot_take` | 拍快照（攻击前落基线） | 低 |
| `vm_snapshot_restore` | **还原快照（靶场重置核心）**，需 confirm | 高 |
| `vm_exec` | guest 内执行命令取回输出（RCE 效果验证核心），需 GA + 凭据 | 高（内容取决于命令） |
| `vm_screenshot` | VM 画面截图（取证），返回宿主机侧路径 | 只读 |
| `vm_copyto` | 投送文件进 guest（payload/取证脚本），需 GA + 凭据 | 中 |
| `vm_copyfrom` | 回收 guest 文件（战果取证），需 GA + 凭据 | 只读 |

## 配置（MCP 环境变量）

### 驱动（VM_DRIVER，默认 auto）

| 驱动 | 场景 | 额外变量 |
|---|---|---|
| `local` | 本机 Linux/Mac 装 VirtualBox | `VM_VBOXMANAGE`（可选，VBoxManage 全路径） |
| `win-interop` | **WSL2 平台 + Windows 宿主 VBox**（本仓库作者主场景） | `VM_WIN_VBOXMANAGE`（可选；默认探测 /mnt/c|d|e 下 Program Files/Oracle/VirtualBox/VBoxManage.exe） |
| `ssh` | 远程 Linux VBox 宿主 | `VM_SSH_HOST` 必填；`VM_SSH_PORT`、`VM_SSH_USER`、`VM_SSH_KEY` 可选 |

`auto` 探测顺序：win-interop（本进程在 WSL 且能找到 exe）→ local。

### guest 凭据（vm_exec / vm_copyto / vm_copyfrom 需要）

- `VM_GUEST_USER` / `VM_GUEST_PASSWORD`：全局默认；
- `VM_GUEST_CREDENTIALS`：按 VM 覆盖，JSON：`{"win7-lab": {"user": "admin", "password": "…"}}`。

凭据只进 MCP 运行时环境（平台侧存 DB `mcp_servers.env`），不写入仓库/报告/日志。VBoxManage 以 `--passwordfile`（临时文件 0600，用完即删）传递，避免出现在 argv/ps 里。

### 文件路径参数（截图/copyto/copyfrom）

**关键：这些路径是 VBox 宿主机侧的路径，不是 MCP 进程视角。**

- `local` 驱动：就是本机路径，无坑；
- `win-interop`：VBoxManage.exe 在 Windows 上执行——`VM_SCREENSHOT_DIR`（默认 `./vm-screenshots`）与 copyto 源路径都应给 **Windows 路径**（如 `C:\Users\you\vm-shots`），WSL 侧可通过 `/mnt/c/...` 访问产物用于 save_evidence 归档；
- `ssh`：路径在远程宿主上；截图产物留在远端，工具返回会明确提示。

## 前提条件

1. VBoxManage 可达（对应驱动）；
2. `vm_exec`/`vm_copy*`：靶机装了 **Guest Additions**，且有可登录的 guest 账户；未装时这些工具返回明确报错，其余工具（快照/截图/生命周期）不受影响；
3. 靶机网络：平台与靶机网络互通（NAT/Host-Only/桥接均可），Web 类验证用 Playwright MCP 直接访问靶机 IP。

## 安全模型

- VBoxManage argv 全部由代码按白名单形态构造，不执行自由命令串；
- VM 名/快照名走正则校验 + 存在性校验，防参数注入；
- 平台内所有调用经 guard 解包后进入三层拦截（deny/judge/ask）——`vm_snapshot_restore`、`vm_stop(poweroff)` 这类操作建议在系统里配 ask 规则转人工审批；
- per-VM 串行锁，避免并发操作撞 VBox 机锁。
