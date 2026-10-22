# 调用后释放与按需唤醒

日期：2026-10-22。实现基线：`5d3d60e494b9e56fa61ab3a5bb80bdcfc5a78ca9`（v0.6.0）。

## 目的

原来的 per-call 在下一次工具调用前更换后端，最近一次调用的 gopls 在空闲期间继续驻留。这是原有生命周期约定，不能单凭驻留认定内存泄漏。本次增加独立的生命周期选项，以减少代理管理的后端空闲占用。

默认 `--backend-lifecycle=resident` 保持原行为。显式指定 `--refresh-mode=per-call --backend-lifecycle=on-demand` 才启用释放。普通交互的空闲期限由 `--backend-idle-timeout` 控制，默认 10 秒且必须为正数；它不是经过实测确定的最优值。

## 状态与边界

| 条件 | 动作 |
|---|---|
| 没有需要后端处理的客户端消息 | 不启动后端 |
| 工具终结帧成功写入前端且相关交互结束 | 协议允许时立即清理，不额外等待空闲期限 |
| 初始化或普通交互完成 | 无工作时启动空闲计时，有新需求则服务需求 |
| 工具完成时已有普通请求排队 | 先退休该工具后端，再为队列唤醒下一代 |
| 清理成功且没有需求 | 保留前端会话，进入无后端状态 |
| 休眠时文件变化 | 留待下次完整内容检查，不启动后端 |
| 新需求唤醒 | 新的恢复预算开始；请求总期限仍从入队计算 |
| 已派发操作取消、超时、崩溃 | 保留原有错误语义，不自动重放 |
| 已写出成功结果后清理失败 | 终止继续服务，不重复回复已完成调用 |

每个工具队项仍须使用为该队项启动的新 boot；取消队项不能把其 fresh boot 转让给后续工具。普通请求唤醒的后端也不能直接作为下次工具调用的新后端。

释放检查独立于刷新恢复预检查。未完成初始化、未闭合目录分页、显式资源订阅、现代长期订阅、任务能力或任务交互、`input_required`、未知与不确定状态会阻止自愿释放。无法证明已解除的条件在本前端会话内保持阻塞。它们不改变现有 per-call 强制刷新规则，也不承诺恢复任意进程内业务句柄。

结果排入输出队列不等于完成交付；需要 writer 成功写出整个帧，并等待双向待写消息、扫描、反向请求、内部控制及结果缓冲结束。stdio 没有客户端消费 ACK，此处只保证写入管道成功。

## 可见的成本

- 工具调用后跟随 `ping` 或 `tools/list` 可能增加一次冷启动；连续两个工具调用仍各用一个新后端。
- `ping` 继续验证真实后端，频繁心跳可能使普通后端持续驻留。
- 无活动显式订阅时，目录 `listChanged` 能力本身不阻止休眠。休眠期间目录推送可能延迟，唤醒时先执行原有目录恢复规则。已构造的旧目录请求不自动改写。
- 通用后端可能把业务句柄放在不透明结果里；需要持续在线状态的使用方保留 `resident`。

## 进程与测量

运行时复用原有 EOF、TERM、KILL、Wait 和 Factory 清理屏障；未确认主后端清理完成不得启动下一主后端。默认 shutdown-timeout 仍为每阶段 2 秒，cleanup-timeout 为总计 6 秒。

telemetry 可能使用独立 session，因此主进程组清理不等于完整树退出。外部观察器在父进程仍存活时记录 PID、父 PID、进程组、session、启动身份及归属，父进程退出后继续追踪已归属进程。侧进程有单独的 6 秒退出观察预算。观测错误或覆盖不足不能算作零残留；这套检查不提供任意 daemonize 后端的运行时树清理保证。

RSS 按同一采样时点分组相加，不相加各进程在不同时间的峰值。进程驻留比例、唤醒次数、工具与恢复耗时和错误应与 RSS 一起解释。race 用例只验证正确性，不能作为正式性能基线。直接 gopls、resident 与 on-demand 的严格收益比较，以及用户实际 Codex 心跳和大工作区测量，仍须在相同环境单独执行。

不设置固定 `GOMEMLIMIT`，不修改用户级 telemetry 模式，不清理其他连接拥有的 gopls。代理自身仍保留 watcher、账本和固定 tick，动态 timer 与多实例全局预算属于后续 P1/P2。

## 验证与启用

原有 resident/per-call 普通请求复用用例保留。新增 raw、组件及真实 gopls 生命周期用例，CI 保持 Linux/macOS × gopls v0.23.0/v0.24.0，完成性检查要求指定用例实际执行且通过；skip 不算通过。CI 保存测试 JSON、过程观测和版本信息。

```sh
# 确定性的协议、状态机和进程回收测试
go test -race ./cmd/... ./internal/... -count=1
go test -race ./integration -run '^TestRaw' -count=1

# 真实 gopls 的有限次数对照与进程归属观测
GOPLS_INTEGRATION=1 GOPLS_EXPECTED_VERSION=v0.24.0 \
  GOPLS_BIN="$(command -v gopls)" PROXY_TEST_EVIDENCE_DIR=/tmp/lifecycle-evidence \
  go test -json ./integration -run '^TestRealGoplsOnDemand(Lifecycle|KilledParentSidecarExit)$' -count=1

# 延长同一个测试到每种模式 100 次调用
GOPLS_INTEGRATION=1 GOPLS_EXPECTED_VERSION=v0.24.0 GOPLS_LIFECYCLE_CYCLES=100 \
  GOPLS_BIN="$(command -v gopls)" PROXY_TEST_EVIDENCE_DIR=/tmp/lifecycle-soak \
  go test -json ./integration -run '^TestRealGoplsOnDemand(Lifecycle|KilledParentSidecarExit)$' -count=1 -timeout=40m
```

手动工作流 `Lifecycle soak profile` 在两平台、两版 gopls 上执行延长测试。默认每种模式 100 次，允许 3–1000 次；它是小工作区、单会话的正确性与稳定性采样，不代替生产工作负载的内存收益实验。进程采样中的 `-1` FD 数表示平台不提供该观测；不能当作零。

合入或启用前应完成真实后端至少 100 次工具调用循环，并在实际 Codex 连接验证连续调用。CI 和 fixture 结果不代替用户 Mac 的宿主验收。本次不修改本机 Codex 配置，也不承诺原采样中两个直连 gopls 的约 744 MiB 会被代理回收。

启用配置见 README。回退时改为 resident 并重新建立 MCP 连接；per-call 的每次新进程语义保留。
