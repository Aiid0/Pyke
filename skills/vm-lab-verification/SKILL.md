---
name: vm-lab-verification
description: 挖到漏洞（尤其 RCE/提权/文件上传）之后、写 report_finding 之前调用——用 VirtualBox 靶机做攻击效果验证与取证：快照基线、guest 内命令取证（vm_exec）、VM 截图、payload 投送与战果回收、验证完还原靶场。目标环境是隔离 lab 靶机（用户自有授权环境）时使用。
mcps: vm-lab
tags:
  cwe: []
  attack: [rce, privilege-escalation, post-exploitation]
  class: [verification, evidence, helper]
---

# VM Lab 攻击效果验证（VirtualBox 靶机）

平台闸门要求 critical/high 必须有 L3+ 证据（命令命中输出 / 流量对比 / 状态变化）。
「拿到 RCE」只到 L1/L2；用这份流程把证据推到 L4（数据/影响面）。前提：目标已确认
是**隔离 lab 靶机**（用户自有授权环境）；不确定就先问，不拿生产系统做这些动作。

## 先决检查

1. `vm_list` 确认靶机存在、是否在运行、有没有基线快照；
2. 没有 baseline 快照：先 `vm_snapshot_take`（建议名 `baseline-clean`）——**攻击
   前没有快照，后面就没有干净的重置点**；
3. 靶机网络：两台靶机已桥接宿主 `eno1`，直接上局域网（192.168.3.x，如
   vulnlab=192.168.3.100）——Web 类验证（Playwright/Bash/http_request）直接打
   VM 的局域网 IP，无需端口转发；IP 变了用 `vm_exec ip a`（有凭据时）或
   guestproperty 重新确认；
4. `vm_exec` 报凭据/GA 错误时：回退用平台既有手段（回连 shell + listener_sessions
   留证 + Web 层 Playwright 验证），不要卡死在这一条路上。

## 标准验证节奏（每轮）

```
快照基线(baseline-clean) → 攻击 → 效果取证 → 证据归档 → 还原(clean) → 下一轮
```

1. **攻击**：用平台既有工具（http_request / session_login / Bash）打靶机；
2. **效果取证**（证明攻击真的生效了，不是「我猜它生效了」）：
   - `vm_exec`：`["/c", "whoami & ipconfig & net user"]`（Windows）或
     `["-c", "id; ls -la /tmp; crontab -l; ps aux | grep -i backdoor"]`（Linux）——
     身份、网络位、持久化痕迹逐项取证；
   - `vm_screenshot`：拿桌面证据（登录后的界面、被篡改的页面、弹窗）；
   - `vm_copyfrom`：把落地的文件（webshell / 数据样本）取回归档；
   - Web 效果：browser（Playwright）直接访问靶机 IP 验证功能面变化；
3. **证据归档**：截图/输出按平台纪律存 runDir → `save_evidence` 绑定 finding_id，
   报告里 `![说明](artifact:<id>)` 引用；`report_finding` 的 PoC 命令要能机械复现；
4. **还原**：`vm_stop`（acpi）→ `vm_snapshot_restore`（**必须 confirm=true**，
   平台可能弹人工审批）→ 回到 baseline。还原会销毁现场——确保证据已归档再还原。

## RCE 视觉证据标准流程（弹计算器判据）

「靶机桌面弹出计算器/执行指定无害命令」是仓库认可的 RCE 确认标准。四件套证据链，
**缺任何一件都降级**——弹窗截图单独不构成 RCE 证明：

1. **远程触发**（机械凭据来源）：用 `http_request` 走真实网络路径发 payload——
   这一步的 HTTP 回显（status/响应特征）就是 report_finding 闸门要求的机械验证
   凭据，且全程过 MITM 留痕；**禁止用 vm_exec 触发攻击来冒充远程利用**
   （它是平台自己的 guest 通道，证明不了「远程可利用」）；
2. **桌面视觉证据**：`vm_screenshot` 拍到弹窗本身（Windows: calc.exe/记事本/命令
   窗口；Linux: `DISPLAY=:0` 下 xmessage/gedit/终端回显；无 GUI 靶机改用 Web 面
   变化 + 文件落地 + vm_exec 状态链替代，如实说明无桌面证据的原因）；
3. **状态取证**：`vm_exec` 查进程清单/落地文件/持久化痕迹，把影响推到 L4；
4. **归档**：`save_evidence` 绑 finding_id，note 写清「哪个靶机、什么弹窗、证明哪
   条 RCE」。**报告落位**：截图引用 `![说明](artifact:<id>)` 由 reporter 写进详细
   报告的「## 证据 / ## 复现步骤」小节、**与 PoC 同节并列**（docx 导出原位内嵌；
   详情页/报告页已支持内联渲染）；**不要把引用写进 report_finding 的 evidence
   字段**——那里导出时固定渲染成纯代码块，图会变成字面文本。

**会话语义（最容易翻车的点）**：被利用的服务多跑在服务账号/session 0，payload
直接 `calc.exe` 弹在**不可见**的桌面里——截图拍到空白桌面 ≠ 没弹。落桌技巧：

```powershell
# Windows（payload 或 vm_exec 内执行）：schtasks /IT 把 GUI 进程落到交互桌面
schtasks /create /tn vpop /tr "calc.exe" /sc once /st %SCHTASK_TIME% /it & schtasks /run /tn vpop
# Linux（有 GUI 的靶机）：at / nohup + DISPLAY=:0
DISPLAY=:0 nohup xmessage RCE-PROOF >/dev/null 2>&1 &
```

`vm_exec`（Guest Additions）启动的进程同样不在可见桌面——同样用上面的计划任务
技巧落到桌面。截图时机：payload 触发后**等 2~3 秒**再截，连拍 2 张防时序问题
（`vm_screenshot` 调两次即可）。

**闸门提醒**：report_finding 的机械证据信号表不认截图/artifact——critical/high
的 evidence 必须同时含 HTTP 层机械凭据（第 1 步的回显），截图是视觉佐证。

## 纪律

- **取证在前，还原在后**：restore 不可逆，销毁现场前确认证据已落库；
- **不跨范围**：vm-lab 只对 lab 靶机；目标写回守卫会拦越界，但纪律在先；
- **不假设成功**：vm_exec 输出为空 ≠ 验证失败，换手段再试（≥2 种本质不同的
  验证手段，见 pentest-verification 铁律）；
- **凭据卫生**：guest 凭据只在 MCP 配置里，不写进 fact/finding/报告文本。
