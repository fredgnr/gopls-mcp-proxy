# CI 性能监控

本仓库沿用 [reqws-desktop 的 runner 监控方案](https://github.com/fredgnr/reqws-desktop/tree/80f91d952825c5aa1b18e2e3f920e12dc17bb54b/.github)，帮助定位 CI 中 CPU 饱和、内存压力、磁盘和网络开销。它记录整个 runner 的资源使用；已有的 `BenchmarkContentScan` 和 `TestPerformanceProfile` 仍按原方式手动运行。本次没有为产品性能建立历史基线或回归阈值。

## 覆盖与查看方式

| 工作流 | 监控任务 |
|---|---|
| `ci.yml` | Linux/macOS、Go/gopls 测试矩阵的每个实例 |
| `build.yml` | CI 或发布流程调用的四平台二进制构建与打包任务 |
| `release.yml` | tag 发布任务；其验证与构建由上面两个 reusable workflows 覆盖 |

触发条件不变。普通分支 push、PR 和手动 CI 运行都会采集；tag 发布流程在相应任务运行时采集。只调用 reusable workflow 的 job 没有自己的 runner，因此不重复启动监控。

1. 打开 GitHub Actions 中对应的一次运行，在 Job Summary 的 **Workflow Telemetry** 中查看步骤耗时和资源曲线。Summary 由 action 的 post 阶段生成。
2. 在同次运行的 Artifacts 中下载 `telemetry-<run-id>-<attempt>-<job>-<随机后缀>`，读取 `metrics.json`。随机后缀避免矩阵实例或重跑覆盖彼此，保留期为 7 天。
3. JSON 的 `identity` 包含仓库、workflow、run ID/attempt、commit SHA 和 runner 身份。矩阵实例可用 `RUNNER_NAME` 与该次 attempt 的 Actions jobs 对应；PR 中 `GITHUB_SHA` 是被检出的 merge ref，不应当作 PR head。

JSON 在普通步骤结束时导出 action 已采集的数据，不含最后一个尚未完成的采样间隔、之后的上传或 post actions。它与稍后生成的 Summary 范围不同。checkout 失败、runner 被强制终止或 job 超时时，无法保证导出文件与 Summary；可正常执行收尾步骤的测试失败仍会尝试导出。

## 指标口径

固定使用 `ycfreeman/workflow-telemetry-action@72ec425db8a31670fdc4419966940a6d980a0b49`（v3.0.2），每 5 秒采样一次。

| JSON 指标 | 含义与单位 |
|---|---|
| `cpu` | 总、用户态、系统态 CPU 负载，百分比 |
| `memory` | 总、活跃、可用内存，MiB |
| `network` | 每个采样间隔接收/发送的数据量，MiB，不是 MiB/s |
| `disk` | 每个采样间隔读取/写入的数据量，MiB，不是 MiB/s |
| `disk_size` | 已用/可用磁盘空间，MiB |

每项都有 `status`：有样本为 `available`，空数组为 `no-samples`，请求或数据校验失败为 `unavailable`。时间戳为 UTC Unix 毫秒。各序列依次读取，边界可能略有不同。

上游 I/O 值会向下取整为整数 MiB；不支持的传感器也可能报告零。因此零不能证明完全没有活动。数据包含 runner 上其他进程的消耗，不能解释成 gopls 单进程峰值或某个步骤的独占资源。Hosted runner、系统镜像和缓存状态会变动，跨运行比较需结合这些差异。

## 采集边界与维护

- collector 放在每个 runner job 的第一步，覆盖 checkout、依赖安装、测试和构建。
- 开启 Job Summary，关闭 PR 评论和进程 trace 图表/表格输出。上述进程选项只控制展示，不代表 action 从未启动进程 tracer。
- `actions: read` 用于读取 job 元数据；验证与构建仍为只读，发布任务保留原有 `contents: write`。不增加 PR 写权限，checkout 继续使用 `persist-credentials: false`。
- 导出脚本仅访问本机 collector 的五个固定端点，不携带 token，不使用 HTTP proxy，不跟随重定向。每次请求超时 2 秒、响应上限 4 MiB，只保留白名单数字指标和公开运行身份；失败只保存异常类型。
- artifact 只上传生成的 JSON 文件，不打包日志、环境变量或进程参数。采集、导出和上传步骤均允许失败，业务测试与构建仍保持原有失败行为。导出与上传分别限制 1 分钟、2 分钟。

导出器单元测试在最低 Go 版本的 CI 矩阵实例执行，也可本地运行，无需安装 Go 或 Python 第三方库：

```sh
python3 -B -m unittest discover -s scripts/tests -p 'test_telemetry_export.py' -v
```

更新 collector pin 时，需要同时核对其五个端点的字段、单位和 post 行为，更新脚本中的 `COLLECTOR`，再验证 Linux/macOS 的实际 Summary、导出步骤与 artifact。
