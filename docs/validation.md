# 验证范围

> 文档归属：本文记录首版测试和历史验证结果。后续改造的验收要求见 [新方案验收计划](changes/proxy-refresh-protocol/validation.md)，其中明确替换“内容编辑后 generation 不变”等旧断言。新要求尚未执行，不应将下列历史通过结果视为新方案已通过。

## 自动化场景

| 设计验收项 | 验证位置 |
|---|---|
| 依赖内容变化、mtime 不变、新增空依赖文件 | `internal/workspace/tracker_test.go` |
| Go 文件增删改名、原子保存、目录移动 | `internal/workspace/tracker_test.go` |
| go.work 外部模块、显式 replace 目录、扫描边界 | `internal/workspace/tracker_test.go` |
| 300 个生成文件的事件合并 | `TestAtomicSaveAndGeneratorBurst` |
| 完全丢失监听事件后仍切换后端 | `TestMissingEventsTwoDependencyUpdatesSingleClient` |
| fsnotify 与轮询分别检测变化 | `TestWatcherAndPollingEachDetectChanges` |
| 长请求结果过期、不自动重放 | `TestOldResultDiscardedWithoutReplay` |
| 启动期间再次变更、候选后端被丢弃 | `TestChangesDuringStartAreNeverPublished` |
| 有界队列、取消释放、等待超时、重建中 ping | `TestQueueCancellationLimitsAndPingDuringRebuild` |
| 崩溃恢复、无效 go.mod 修复、schema 变化拒绝 | `TestCrashLoadingFailureAndSchemaDrift` |
| 执行总期限、排空期限 | `TestExecutionTimeoutAndBoundedDrain` |
| 真实 MCP 子进程的 roots、进度映射、完整结果 | `internal/backend/backend_test.go` |
| 下游取消通知、进程崩溃、初始化无响应、Wait 回收 | `internal/backend/backend_test.go` |
| 同一代理进程/同一 MCP 会话两次 protobuf 升级 | `integration/real_gopls_test.go` |
| 普通内容编辑由 gopls 更新、generation 不变 | `integration/real_gopls_test.go` |

真实集成测试使用 `go get` 进行依赖升级，并检查 `go_file_context` 返回的声明路径包含对应 `protobuf@版本/`。这同时验证双 MCP 会话、重建、指纹核对和结果转发，而不只检查进程是否重新启动。

## 首次实现的本地结果

2026-10-19，在 Linux 环境使用 Go 1.26.1 构建代理、gopls v0.24.0（由自动下载的 Go 1.27.2 构建）：

- `go test -race ./...`：通过；真实集成测试在此命令中按设计跳过。
- `go vet ./...`：通过。
- `GOPLS_INTEGRATION=1 GOPLS_BIN=... go test -v ./integration -count=1`：通过。
- 在同一个 MCP 连接和同一个代理 PID 中，依次观察到 protobuf v1.34.2、v1.35.2、v1.36.6 的源码路径。

macOS 和最低 Go 版本由仓库 CI 执行，其状态以对应提交的 Actions 结果为准。Windows 尚未做正式验收；Unix 平台使用独立进程组清理 gopls 及其后代，其他平台只提供直接子进程清理。

## 边界

- 不承诺文件系统事务快照；一致性边界是请求派发前和返回前的校验。
- 任意目录规模下的吞吐量未做基准测试。请求前后全量路径遍历可能增加大仓库延迟。
- 连续写入跨越安静窗口可形成多次有效重建；生成器单个事件批次的合并由受控测试验证。
- 目录符号链接不隐式扩展扫描范围；外部本地依赖需通过 go.work 或 `--replace-dir` 纳入。
