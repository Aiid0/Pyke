# LibAFL 作业

任务详情的「Fuzz 作业」与 Worker 的 fuzz_* 工具使用相同后台。支持本地 C/C++ harness，入口为 LLVMFuzzerTestOneInput，链接 LibAFL runtime；不接受普通未插桩二进制。

## 部署

```bash
docker build -t pyke-libafl:0.15.4 fuzzing/runner
```

服务配置 `PYKE_FUZZ_IMAGE=pyke-libafl:0.15.4` 后重启。未配置时功能可见但不执行。服务进程需要 Docker CLI 和容器运行时访问权限。Docker 部署时显式提供运行时访问及数据卷；`PYKE_FUZZ_HOST_ROOT` 为 Docker daemon 所在宿主上对应 Pyke 数据目录的绝对路径。启动前会通过随机内容摘要校验共享目录映射。

镜像固定 LibAFL commit bd49bdf4f64a50349d803646fb2afa9b8b104fa5、Rust nightly-2025-10-01；镜像内保存 Cargo.lock。具体作业记录镜像内容 ID 和源码 SHA256 摘要。运行环境不会自动安装或下载项目依赖，额外依赖应提前加入派生镜像或源码目录。

正常流程由 Agent 自动准备 `shared/fuzz-targets/<name>/`，并写入 `pyke-fuzz.json`；后台发现后自动创建作业，用户只需在任务详情查看状态。手工调试时仍可将 fuzzing/runner/example 复制到任务工作目录并用 source=shared/parser-example、build=build.sh、seeds=seeds 创建作业。该示例包含故意的越界写，只用于验证链路。

## 行为

每任务并发 1、全局并发 2；默认 1 CPU/2 GiB、构建 10 分钟、运行 30 分钟。表单/API 可调资源预算（CPU 1–4、内存 256–16384 MiB、运行 1–86400 秒）。输出增长默认限 1 GiB，监督进程周期检查并停止；这是软限制，不是文件系统硬配额，部署磁盘应留余量。

任务目录 shared/fuzz/<id> 保存源码快照、配置、构建产物、语料、事件和样本。停止整个容器，继续时创建关联的新作业并复用语料。Agent 对话结束不停止 fuzz；任务暂停/删除停止，恢复任务不会自动重启停止的 fuzz。

新异常默认待人工处理。自动分析开启后，每作业每分钟最多一个、总计最多 5 个分析意图；暂停时积压。重复点击分析复用意图，Worker 按证据区分目标缺陷与 harness/环境问题，并通过现有漏洞和附件工具登记。

候选先按输入哈希去重，根因归并由回放与分析确认。覆盖反馈计数不等于源码行覆盖百分比。OOM、容器退出与构建失败不会直接登记为漏洞。
