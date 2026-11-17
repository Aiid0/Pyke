---
name: fuzz-workflow
description: 使用任务级 LibAFL 对本地 C/C++ 源码、库函数和解析器进行覆盖率引导测试，准备 harness、种子与构建脚本，分析并回放异常样本时调用。
---

# LibAFL 作业

先通过 fuzz_list 检查同目标已有作业，复用结果。主 Agent 和 Planner 派意图给 Worker 准备和运行；Worker 使用 fuzz_start，作业独立于本次回合继续运行。

## 准备入口

在任务目录的独立子目录（如 shared/parser-fuzz）准备源码、harness、build.sh、seeds/，不修改绑定源码。源码快照内不能含软链接。记录原始版本和目标入口。

harness 导出 `extern "C" int LLVMFuzzerTestOneInput(const uint8_t*, size_t)`；每次调用正确初始化并释放状态，遵守目标 API 前置条件。避免仅测试 harness 自身或在到达解析逻辑前拒绝所有输入。

build.sh 在源码副本内执行，必须生成 `$OUT/fuzz_target`。运行环境提供 `CC`、`CXX`、`CFLAGS`、`CXXFLAGS` 和 `LIBAFL_LIB`，链接示例：

```bash
${CXX} ${CXXFLAGS} harness.cc target.cc "${LIBAFL_LIB}" -lpthread -ldl -lm -o "${OUT}/fuzz_target"
```

依赖需要提前放入源码或部署镜像；执行环境不联网。提供至少一个小于 max_input 的真实格式种子。构建缺依赖时修复准备目录再创建新作业。

## 调用与反馈

`fuzz_start` 的 source 相对任务目录；build、seeds、dictionary 相对 source。每个新作业使用新的 request_id，同一请求重试复用原 ID。默认 30 分钟、1 CPU、2 GiB；auto_analyze 默认关闭，只有用户希望自动分析时开启。

返回 ID 表示已排队，不表示验证完成。用 fuzz_status/fuzz_results 检查实际运行与证据。启动后继续其他工作，不反复空轮询。Agent 回合结束不会停止作业，任务暂停会停止它。

## 异常分析

异常组包含样本哈希、日志、证据目录和分析意图。先 fuzz_replay 创建独立回放，记录回放作业 ID；待结果就绪再判断是否重复触发。回放自身不会自动递归创建分析任务。

区分目标缺陷、harness 违反 API 约束、运行环境错误、超时与证据不足。容器 OOM 不等于目标内存漏洞。不同输入可能来自同一根因，相同信号不证明根因相同。

确认缺陷时记录源码版本、harness、构建条件、最小化样本、独立回放与影响证据；按现有 report_finding 流程登记，使用 save_evidence 登记样本与日志，注明 fuzz_job_id/group_id。不能仅凭 crash 候选升级为 confirmed。分析未结束、未复现和预算耗尽应如实说明。
