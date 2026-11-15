"""VM Lab MCP server (stdio)——VirtualBox 靶机控制与攻击效果验证.

用途：挖到漏洞（尤其 RCE）之后，验证攻击效果并取证。围绕三条能力线：
  1. 靶场重置：vm_snapshot_take / vm_snapshot_restore——攻击前落基线快照，
     验证完还原，靶场永远干净（这是整个工具集存在的理由）；
  2. 效果取证：vm_exec（Guest Additions guestcontrol，查进程/文件/持久化）、
     vm_screenshot（VM 显示画面截图）、vm_copyto/copyfrom（投送 payload /
     回收战果文件）；
  3. 生命周期：vm_list / vm_start / vm_stop。

安全边界（实现内强制，不是靠提示词）：
  - 只构造白名单内的 VBoxManage 固定 argv 形态，不接受自由命令串；
  - VM 名校验 ^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$ 且必须存在于 vm_list 结果；
  - vm_stop(poweroff) / vm_snapshot_restore 属破坏性操作，必须显式 confirm=true；
  - guest 凭据只进环境变量（DB mcp_servers.env），不入仓库不入报告。

驱动（VM_DRIVER，默认 auto 自动探测）：
  - local       本机 VBoxManage（PATH，或 VM_VBOXMANAGE 指定全路径）
  - win-interop Windows 宿主上的 VBoxManage.exe（WSL2 interop 直调；
                VM_WIN_VBOXMANAGE 可指定路径，默认探测常见安装位）
  - ssh         远程 Linux 宿主：VM_SSH_HOST/PORT/USER/KEY + 远端 VBoxManage
  注意：win-interop/ssh 驱动下，文件类参数（截图目录、copyto 源路径）是
  **VBox 宿主机侧的路径**，不是本进程视角的路径——README 有逐驱动说明。

模拟点击分层：Web 应用点击走平台的 Playwright MCP（browser 预设）直接访问
VM 网段靶机，不需要 guest 内动作；VM 桌面点击（二期）用 vm_exec 在 guest 内
跑自动化脚本（pyautogui / PowerShell SendKeys），本 server 不注入鼠标事件。
"""

from __future__ import annotations

import json
import os
import re
import shlex
import subprocess
import tempfile
import threading
import time
from pathlib import Path
from shutil import which
from typing import Any

from mcp.server.fastmcp import FastMCP

mcp = FastMCP("vm-lab")

VM_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$")
EXEC_TIMEOUT_MAX = 600
CTRL_TIMEOUT_DEFAULT = 30

# per-VM 串行闸：同一台 VM 同时只跑一个 VBoxManage 操作（guestcontrol 与
# snapshot/.controlvm 并发会撞 VBox 的机锁）；不同 VM 之间不互相阻塞。
_vm_locks: dict[str, threading.Lock] = {}
_vm_locks_mu = threading.Lock()


class VMError(Exception):
    """面向 agent 的错误：带原因与下一步指引，文本直接进 tool_result。"""


def _env(key: str, default: str = "") -> str:
    return os.getenv(key, default).strip()


def _vm_lock(vm: str) -> threading.Lock:
    with _vm_locks_mu:
        if vm not in _vm_locks:
            _vm_locks[vm] = threading.Lock()
        return _vm_locks[vm]


# ---------------------------------------------------------------- 驱动层


def _detect_driver() -> tuple[list[str], str]:
    """返回 (argv 前缀, 描述)。前缀后面直接拼 VBoxManage 参数。"""
    driver = _env("VM_DRIVER", "auto").lower()

    def win_candidates() -> list[str]:
        out = []
        if p := _env("VM_WIN_VBOXMANAGE"):
            out.append(p)
        for root in ("/mnt/c", "/mnt/d", "/mnt/e"):
            out.append(f"{root}/Program Files/Oracle/VirtualBox/VBoxManage.exe")
        return out

    def probe(p: str) -> bool:
        return os.path.isfile(p) or (os.sep not in p and which(p) is not None)

    if driver == "local":
        p = _env("VM_VBOXMANAGE") or "VBoxManage"
        return [p], f"local:{p}"
    if driver == "win-interop":
        for p in win_candidates():
            if probe(p):
                return [p], f"win-interop:{p}"
        raise VMError(
            "win-interop 驱动未找到 VBoxManage.exe。请确认 /mnt/c 挂载与安装路径，"
            "或在 MCP 环境变量 VM_WIN_VBOXMANAGE 里填全路径（如 "
            "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe 对应的 /mnt/c/... 形式）。"
        )
    if driver == "ssh":
        host = _env("VM_SSH_HOST")
        if not host:
            raise VMError("ssh 驱动需要 VM_SSH_HOST（VBox 宿主机地址）。")
        argv = ["ssh", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new"]
        if port := _env("VM_SSH_PORT"):
            argv += ["-p", port]
        if key := _env("VM_SSH_KEY"):
            argv += ["-i", key]
        user = _env("VM_SSH_USER")
        target = f"{user}@{host}" if user else host
        return [*argv, target], f"ssh:{target}"
    # auto：win-interop（本进程是 WSL 且能找到 exe）→ local
    for p in win_candidates():
        if probe(p):
            return [p], f"win-interop:{p}"
    return ["VBoxManage"], "local:VBoxManage"


def _vbox(*args: str, timeout: int = CTRL_TIMEOUT_DEFAULT) -> str:
    """执行一条 VBoxManage 命令，成功返回 stdout，失败抛 VMError。

    禁止 shell=True、禁止拼接字符串——所有参数走 argv 列表，驱动侧做转义。
    """
    prefix, desc = _detect_driver()
    if prefix[0] == "ssh":
        remote = "VBoxManage " + " ".join(shlex.quote(a) for a in args)
        argv = [*prefix, "--", remote]
    else:
        argv = [*prefix, *args]
    try:
        cp = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise VMError(f"VBoxManage 超时（{timeout}s）：{' '.join(args[:2])}。VM 可能卡在异常状态，可 vm_list 看状态。")
    except FileNotFoundError:
        raise VMError(f"找不到 VBoxManage（驱动 {desc}）。请装 VirtualBox 或配置 VM_DRIVER/VM_VBOXMANAGE。")
    if cp.returncode != 0:
        err = (cp.stderr or cp.stdout or "").strip()
        raise VMError(f"VBoxManage 失败（{' '.join(args[:2])}）：{err or f'退出码 {cp.returncode}'}")
    return cp.stdout


# ---------------------------------------------------------------- 解析与校验


def _parse_vms(text: str) -> list[dict[str, str]]:
    """解析 `list vms` 输出：'Name' {uuid} 每行一条。"""
    out = []
    for line in text.splitlines():
        m = re.match(r'^"(.+)"\s+\{([0-9a-fA-F-]+)\}\s*$', line.strip())
        if m:
            out.append({"name": m.group(1), "uuid": m.group(2)})
    return out


def _parse_machinereadable(text: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for line in text.splitlines():
        if "=" in line:
            k, _, v = line.partition("=")
            out[k.strip()] = v.strip().strip('"')
    return out


def _resolve_vm(vm: str) -> str:
    """校验 VM 名并确认存在，返回可安全传给 VBoxManage 的标识（名或 UUID）。"""
    if not VM_NAME_RE.match(vm or ""):
        raise VMError(f"VM 名不合法：{vm!r}。允许字母数字、空格、点、下划线、连字符。")
    text = _vbox("list", "vms")
    for entry in _parse_vms(text):
        if vm in (entry["name"], entry["uuid"]):
            return vm
    known = ", ".join(e["name"] for e in _parse_vms(text)) or "（空）"
    raise VMError(f"找不到 VM {vm!r}。当前注册的 VM：{known}")


def _guest_credentials(vm: str) -> tuple[str, str]:
    """guest 凭据：per-VM JSON 覆盖（VM_GUEST_CREDENTIALS）→ 全局 env。"""
    raw = _env("VM_GUEST_CREDENTIALS")
    if raw:
        try:
            table = json.loads(raw)
            if vm in table:
                return table[vm].get("user", ""), table[vm].get("password", "")
        except json.JSONDecodeError:
            raise VMError("VM_GUEST_CREDENTIALS 不是合法 JSON（应为 {\"vm名\": {\"user\":..,\"password\":..}}）。")
    user = _env("VM_GUEST_USER")
    password = _env("VM_GUEST_PASSWORD")
    if not user or not password:
        raise VMError(
            f"缺少 {vm} 的 guest 凭据。请在 MCP 配置的环境变量里提供 "
            "VM_GUEST_USER/VM_GUEST_PASSWORD（或 VM_GUEST_CREDENTIALS 按 VM 覆盖）；"
            "凭据只进运行时配置，不写入任何文件。"
        )
    return user, password


def _passwordfile(password: str) -> str:
    """guestcontrol 需要 --passwordfile；临时文件 0600，用完即删。"""
    fd, path = tempfile.mkstemp(prefix="vmpw-", suffix=".txt")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(password)
        os.chmod(path, 0o600)
    except Exception:
        os.unlink(path)
        raise
    return path


def _screenshot_dir() -> Path:
    d = Path(_env("VM_SCREENSHOT_DIR", "./vm-screenshots")).expanduser()
    d.mkdir(parents=True, exist_ok=True)
    return d


# ---------------------------------------------------------------- 工具


@mcp.tool()
def vm_list() -> str:
    """列出本驱动可管理的全部 VirtualBox 虚拟机及状态（靶场清单）。

    返回每台 VM 的：名称、UUID、电源状态（poweroff/running/saved/aborted）、
    内存、CPU 数、当前快照。攻击前先看清单，确定靶机名与是否在运行。
    """
    entries = _parse_vms(_vbox("list", "vms"))
    if not entries:
        return "当前驱动下没有注册任何 VM。"
    out = [f"共 {len(entries)} 台 VM："]
    for e in entries:
        ident = e["uuid"]
        try:
            info = _parse_machinereadable(_vbox("showvminfo", ident, "--machinereadable"))
            state = info.get("VMState", "unknown")
            mem = info.get("memory", "?")
            cpus = info.get("cpus", "?")
            snap = info.get("SnapshotName", "-") or "-"
            out.append(
                f"- {e['name']}（{state}）uuid={ident} 内存={mem}MB CPU={cpus} 当前快照={snap}"
            )
        except VMError as exc:
            out.append(f"- {e['name']}（uuid={ident}）信息读取失败：{exc}")
    return "\n".join(out)


@mcp.tool()
def vm_start(vm: str, headless: bool = True) -> str:
    """启动一台虚拟机（默认 headless 无界面，后台服务可用）。

    vm 用 vm_list 里的名称或 UUID。headless=false 时带 GUI 启动（需要桌面环境）。
    已在运行的 VM 会得到明确提示而不是报错。
    """
    ident = _resolve_vm(vm)
    with _vm_lock(ident):
        state = _parse_machinereadable(_vbox("showvminfo", ident, "--machinereadable")).get("VMState", "")
        if state == "running":
            return f"{vm} 已在运行，无需启动。"
        mode = "headless" if headless else "gui"
        _vbox("startvm", ident, "--type", mode, timeout=60)
        return f"{vm} 已以 {mode} 模式启动。等几秒让网络起来，可用 vm_list 确认状态。"


@mcp.tool()
def vm_stop(vm: str, mode: str = "acpi", confirm: bool = False) -> str:
    """停止一台虚拟机。

    mode="acpi"（默认）：发 ACPI 关机事件，guest 正常关机——温和，不需要 confirm。
    mode="poweroff"：直接拔电源，guest 文件系统可能损坏——**必须 confirm=true**，
    且会话内应先 vm_snapshot_take 留底。停止后才能 vm_snapshot_restore。
    """
    ident = _resolve_vm(vm)
    mode = (mode or "acpi").lower()
    if mode == "poweroff" and not confirm:
        raise VMError("poweroff 是硬断电（可能损坏 guest 文件系统）。确认请带 confirm=true；"
                      "温和关机用 mode=\"acpi\"（默认）。")
    if mode not in ("acpi", "poweroff", "savestate"):
        raise VMError(f"mode 只支持 acpi/poweroff/savestate，收到 {mode!r}。")
    with _vm_lock(ident):
        if mode == "acpi":
            _vbox("controlvm", ident, "acpipowerbutton")
            return f"{vm} 已发送 ACPI 关机事件，guest 正在自行关机，稍后 vm_list 确认。"
        if mode == "poweroff":
            _vbox("controlvm", ident, "poweroff")
            return f"{vm} 已强制断电。"
        _vbox("controlvm", ident, "savestate")
        return f"{vm} 已保存状态并停止（saved），可再启动。"


@mcp.tool()
def vm_snapshot_list(vm: str) -> str:
    """列出一台 VM 的快照树与当前所在快照（靶场重置的依据）。"""
    ident = _resolve_vm(vm)
    with _vm_lock(ident):
        text = _vbox("snapshot", ident, "list")
    return text.strip() or f"{vm} 没有任何快照。建议先 vm_snapshot_take 落一个干净基线。"


@mcp.tool()
def vm_snapshot_take(vm: str, name: str, description: str = "") -> str:
    """给一台 VM 拍当前状态快照。

    典型用法：靶机刚还原到干净状态后拍一个 baseline（名字建议 baseline-clean），
    之后无论攻击把靶机打成什么样，restore 回来即可继续。
    name 只允许字母数字、点、下划线、连字符。
    """
    ident = _resolve_vm(vm)
    if not re.match(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", name or ""):
        raise VMError(f"快照名不合法：{name!r}。允许字母数字、点、下划线、连字符。")
    with _vm_lock(ident):
        args = ["snapshot", ident, "take", name]
        if description:
            args += ["--description", description]
        _vbox(*args, timeout=120)
    return f"{vm} 快照 {name!r} 已创建。"


@mcp.tool()
def vm_snapshot_restore(vm: str, name: str, confirm: bool = False) -> str:
    """把 VM 还原到指定快照——**靶场重置核心**。

    会丢弃当前磁盘状态（包括验证攻击时落下的文件/后门/改动），把机器拉回拍快照
    那一刻。要求 VM 处于关机或 saved 状态；运行中会被 VBoxManage 拒绝（先 vm_stop）。
    **必须 confirm=true**——这是破坏性操作，误触会毁掉取证现场。
    典型节奏：baseline 拍好后，每轮验证完 restore 回干净态再开下一轮。
    """
    ident = _resolve_vm(vm)
    if not confirm:
        raise VMError("快照还原会丢弃 VM 当前全部磁盘状态（含取证现场）。确认请带 confirm=true。")
    if not re.match(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", name or ""):
        raise VMError(f"快照名不合法：{name!r}。")
    with _vm_lock(ident):
        state = _parse_machinereadable(_vbox("showvminfo", ident, "--machinereadable")).get("VMState", "")
        if state == "running":
            raise VMError(f"{vm} 正在运行，不能还原快照。先 vm_stop（acpi 或 poweroff）再试。")
        _vbox("snapshot", ident, "restore", name, timeout=180)
    return f"{vm} 已还原到快照 {name!r}（当前状态=快照时状态）。启动后即得干净靶机。"


@mcp.tool()
def vm_exec(vm: str, exe: str, args: list[str] | None = None, timeout_sec: int = 60,
            wait: bool = True) -> str:
    """在 guest 内执行一条命令并取回输出——**RCE/提权效果验证的核心工具**。

    前提：VM 已装 Guest Additions；凭据在 MCP 环境变量（VM_GUEST_USER/
    VM_GUEST_PASSWORD，或 VM_GUEST_CREDENTIALS 按 VM 覆盖）。

    参数：
      exe        guest 内**绝对路径**。Windows 例：C:\\Windows\\System32\\cmd.exe；
                 Linux 例：/bin/sh。
      args       参数列表。Windows cmd 包装：["/c", "whoami && dir C:\\"]；
                 Linux shell 包装：["-c", "id; ls -la /root"]。
                 注意 first token after -- 是 argv0，本工具已按 VBoxManage 语义
                 自动拼接（exe 会出现两次，这是 VBoxManage 的要求）。
      timeout_sec 上限 600；wait=false 时只触发不等待（适合长任务）。

    典型验证姿势（拿到 RCE 后）：whoami / ipconfig / 进程清单 / 计划任务 /
    新增文件 / 防火墙规则——逐条取证，配合 vm_screenshot 与 save_evidence 归档。
    """
    ident = _resolve_vm(vm)
    if not exe or len(exe) > 512:
        raise VMError("exe 必须是 guest 内的绝对路径（≤512 字符）。")
    timeout_sec = max(1, min(int(timeout_sec or 60), EXEC_TIMEOUT_MAX))
    argv_args = [a for a in (args or []) if isinstance(a, str) and a]
    user, password = _guest_credentials(ident)
    pwfile = _passwordfile(password)
    try:
        run_args = ["guestcontrol", ident, "run",
                    "--username", user, "--passwordfile", pwfile,
                    "--exe", exe]
        if wait:
            run_args += ["--wait-stdout", "--wait-stderr"]
        run_args += ["--", exe, *argv_args]
        with _vm_lock(ident):
            # guestcontrol 自身没有全局超时参数，靠 subprocess timeout 兜底；
            # 留 10s 余量给 VBox 内部握手。
            out = _vbox(*run_args, timeout=timeout_sec + 10)
    finally:
        try:
            os.unlink(pwfile)
        except OSError:
            pass
    # VBoxManage 把 guest 进程 stderr 混在 stdout 里；wait-process 时退出码
    # 不直接给出，输出里 "Exit code=" 行由 VBox 打印，原样带回即可。
    return out.strip() or f"（{vm} 内命令执行完成，无输出）"


@mcp.tool()
def vm_screenshot(vm: str) -> str:
    """截取 VM 当前画面为 PNG——**非 Web 攻击效果的主要取证手段**。

    返回落盘路径。文件在 **VBox 宿主机侧**（win-interop/ssh 驱动时不是本进程
    视角路径，README 有说明）；拿到路径后用平台的 save_evidence 归档绑定 finding。
    要求 VM 处于运行状态。
    """
    ident = _resolve_vm(vm)
    state = _parse_machinereadable(_vbox("showvminfo", ident, "--machinereadable")).get("VMState", "")
    if state != "running":
        raise VMError(f"{vm} 不在运行（{state}），没有画面可截。先 vm_start。")
    d = _screenshot_dir()
    path = d / f"{time.strftime('%Y%m%d-%H%M%S')}-{re.sub(r'[^A-Za-z0-9._-]', '_', ident)[:40]}.png"
    with _vm_lock(ident):
        _vbox("controlvm", ident, "screenshotpng", str(path))
    return f"截图已保存：{path}"


@mcp.tool()
def vm_copyto(vm: str, host_path: str, guest_path: str) -> str:
    """把 VBox 宿主机上的文件投送进 guest（放 payload / 放取证脚本）。

    host_path 是 **VBox 宿主机侧路径**（local 驱动=本机；win-interop/ssh 见 README）。
    guest_path 是 guest 内目标路径（目录或完整文件路径）。
    需要 Guest Additions + 凭据（同 vm_exec）。
    """
    ident = _resolve_vm(vm)
    user, password = _guest_credentials(ident)
    pwfile = _passwordfile(password)
    try:
        with _vm_lock(ident):
            _vbox("guestcontrol", ident, "copyinto",
                  "--username", user, "--passwordfile", pwfile,
                  host_path, guest_path, timeout=300)
    finally:
        try:
            os.unlink(pwfile)
        except OSError:
            pass
    return f"已投送 {host_path} → {vm}:{guest_path}"


@mcp.tool()
def vm_copyfrom(vm: str, guest_path: str, host_path: str) -> str:
    """从 guest 回收文件到 VBox 宿主机（取回战果：拖下的数据、落地文件样本）。

    方向与 vm_copyto 相反：guest_path 是 guest 内来源，host_path 是宿主机目标
    （文件或目录）。需要 Guest Additions + 凭据（同 vm_exec）。
    """
    ident = _resolve_vm(vm)
    user, password = _guest_credentials(ident)
    pwfile = _passwordfile(password)
    try:
        with _vm_lock(ident):
            _vbox("guestcontrol", ident, "copyfrom",
                  "--username", user, "--passwordfile", pwfile,
                  guest_path, host_path, timeout=300)
    finally:
        try:
            os.unlink(pwfile)
        except OSError:
            pass
    return f"已回收 {vm}:{guest_path} → {host_path}"


def main() -> None:
    mcp.run()


if __name__ == "__main__":
    main()
