# 当前验证范围

本文说明新实现的测试位置和证据边界。首版“内容编辑由 gopls 自刷新、generation 不变”的断言已被替换；首版测试结果不能作为本次改造的验收结果。

完整目标用例见[改造验收计划](changes/proxy-refresh-protocol/validation.md)。下面列出已编写的自动化覆盖，不把测试存在等同于最终提交通过，也不将单个组件子断言扩大为整个 VAL 完成。

## 独立证明与真实互通

`integration/raw_proxy_test.go` 的后端仅在启动时读取输入，后续请求返回启动快照。`TestRawFixtureNeverRefreshesItself` 先直接运行后端，证明同 PID 在文件变化后仍返回旧内容；再通过实际代理子进程验证内容变化触发新 PID/boot/generation，并保持前端连接。

真实 gopls 使用独立 raw 客户端，不依赖生产 MCP SDK。`TestRealGoplsTwoProtobufUpgrades` 包含直连与代理 `tools/list` 对照、同代理会话 protobuf `v1.34.2 → v1.35.2 → v1.36.6` 的源码路径切换，以及内容编辑、新增、删除、原子替换触发代理 generation 增加。它验证实际互通，不替代无自刷新后端的因果证明。

## 自动化覆盖位置

| 范围 | 主要用例/文件 |
|---|---|
| 原始 argv、cwd/env、PATH、分隔符及旧参数迁移 | `cmd/gopls-mcp-proxy/main_test.go`、`TestRawArgumentsEnvironmentAndWorkingDirectory`、`TestRawProxyArgvAndEnvironment` |
| 刷新模式默认值、显式选择、无效参数及本地诊断 | `TestRefreshModeOptions`、`TestLocalDiagnosticsDoNotEnterRuntimeSetup` |
| 每次工具调用的新进程、排队/取消/控制路由与 batch 边界 | `internal/proxy/per_call_test.go`、`integration/per_call_test.go` |
| 真实 gopls 每次工具调用换代及新增/重命名/修改/删除 | `TestRealGoplsPerCallFreshProcessAndFileChanges` |
| raw JSON、typed ID、数字精度、扩展字段、null、短读短写、大帧与 batch | `internal/wire/wire_test.go`、`TestRawProxyPayloadIdentity`、`TestRawProxyLegacyBatch` |
| 常规输入内容、同长度同 mtime 修改、范围/glob/符号链接 | `internal/workspace/content_test.go`、`tracker_test.go` |
| 写入后恢复原值、事件 epoch、事件丢失、扫描竞态/取消/读取错误 | workspace 内容测试、`TestRawProxyIdlePollRefresh`、`TestRawProxyWatcherSameContentInvalidates` |
| 五修订基本重复刷新与连接保持 | `TestRawProxyRefreshEachRevision` |
| 双向 roots、内部初始化隔离、legacy 日志与资源订阅恢复 | `TestRawProxyReverseRootsAcrossRefresh`、`TestRawProxyConfirmedLegacyStateRecovery`、`internal/proxy/recovery_test.go` |
| 目录分页、重复 cursor、不完整目录、未知初始化扩展、约定变化 | `internal/proxy/recovery_test.go`、`TestRawProxyUnknownInitializeRequiresReconnect` |
| 现代 metadata、合法 ID 复用、取消后复用 | `TestRawProxyModernMetadataAndIDReuse`、`TestRawProxyModernCancelAndReuseID` |
| 现代 MRTR 与订阅跨刷新行为 | `TestRawProxyModernMRTRAcrossRefresh`、`TestRawProxyModernSubscriptionRefresh` |
| 已派发业务不重放、写工具副作用报告、未派发取消 | `TestRawProxyNeverReplaysDispatchedBusiness`、`TestRawProxyWriteToolReportsSideEffects`、`TestRawProxyCancelQueuedRequest` |
| TERM 忽略、阻塞管道、Wait、后代进程组、禁止重叠启动 | `internal/backend/backend_test.go`、`process_test.go` |
| 二进制符号链接升级与每次启动摘要 | `TestExecutableLocatorAcceptsSymlinkUpgrade` |
| 100 次连续刷新 | `TestRawProxyHundredRefreshes` |
| 流式内容扫描成本 | `BenchmarkContentScan` |

测试使用受控临时目录、原始客户端与原始后端。race 构建的 raw 集成测试也用 race 构建代理程序。真实 gopls 测试才需要额外安装 gopls 和下载 protobuf；核心 raw 用例不因缺少 gopls 而跳过。

## 协议证据应分开记录

| 修订 | 转发/组件覆盖 | 刷新与恢复覆盖 | 尚不能据此宣称 |
|---|---|---|---|
| `2024-11-05` | profile、原始中继与初始化 | 基本内容刷新；legacy 恢复组件 | 任意客户端/扩展状态都能隐藏恢复 |
| `2025-03-26` | 上述内容及 batch 成员转发 | 基本内容刷新；legacy 恢复组件 | 所有 batch × 取消 × 部分写入组合均通过 |
| `2025-06-18` | profile、原始中继与初始化 | 基本内容刷新；legacy 恢复组件 | 未覆盖能力组合的全面认证 |
| `2025-11-25` | legacy raw 互通与真实 gopls 目标 | 内容刷新、日志/订阅/目录恢复用例 | 单次真实 gopls 通过等于所有 legacy 后端通过 |
| `2026-07-28` | 逐请求 metadata、MRTR、订阅、ID/取消 fixture | 基本刷新、MRTR continuation、旧订阅结束 | 真实 gopls 支持现代协议，或未知状态无损迁移 |

未知修订和未知初始化扩展的正常转发，与刷新时显式重连分别验证。任意 task、业务 handle、opaque continuation 的跨进程持久性不在保证范围内。

## 执行与 CI

```sh
# 默认包含 raw helper 的核心回归。
go test -race ./...
go vet ./...
go build ./cmd/gopls-mcp-proxy

# 单独执行独立刷新与协议用例。
go test -race -v ./integration -run '^TestRaw' -count=1

# 真实互通；需要支持 MCP 的 gopls，并允许下载测试依赖。
GOPLS_INTEGRATION=1 GOPLS_BIN="$(command -v gopls)" \
  go test -v ./integration -run '^TestRealGopls' -count=1

# 内容扫描基准，不包括真实后端启动成本。
go test ./internal/workspace -run '^$' -bench BenchmarkContentScan -benchmem
```

CI 保留 Linux/macOS、最低 Go 1.25、Go 1.27.2、固定 gopls v0.24.0、race/vet/build 和四平台打包。组件测试、raw 合同测试、真实互通分为独立步骤；Linux 最低版本也必须执行 raw 用例。测试将 `PROXY_TEST_EVIDENCE_DIR` 放在 runner 临时目录，保留 fixture 的 boot/receipt/execution/raw 消息事件及代理 stderr，不把证据写入被监测根。JSON 结果和这些轨迹一起作为 `test-results-<os>-go-<version>` artifact 保留 14 天，构建包作为 `release-dist` 保留 14 天。

CI 使用 `contents: read`，checkout 为 `persist-credentials: false`，不使用 `pull_request_target`。本次验证配置不增加 tag、Release 或发布权限。

默认入口开发的最终提交、命令结果和远端 Actions 链接由[默认入口实施记录](changes/default-gopls-launcher/implementation-results.md)与所链接的 PR/CI 登记；该历史记录不作为新增 `per-call` 模式的通过证据。本文不预先宣布尚未运行的最终 Linux/macOS 矩阵通过。

## 明确限制

- 内容扫描基准不能替代完整中继吞吐量、刷新延迟分位数和长期资源收敛测量。100 轮功能测试也不是完整性能验收。
- 目前的用例集合不等于完整 VAL 矩阵；所有版本下的反向交互、背压、部分写入、损坏帧、分页变化及取消竞态仍需逐项核对实际覆盖。
- Linux 本地结果不能替代 macOS Actions，也不能替代最终提交的测试。四架构交叉构建不是四架构运行验收。
- Unix 进程组清理覆盖受管理后代；自行脱离进程组的外部守护程序不在此进程树管理保证内。Windows 未列为运行支持平台。
- 不提供文件系统事务或副作用回滚；未知业务状态、完全未观察到的短暂文件变化不承诺恢复或检测。

## 默认 gopls 启动入口

新增 `internal/launcher` 定位/身份组件、CLI 默认意图和 `backend.Config.BeforeStart` 的逐次检查。独立夹具用真正构建的 proxy 与名为 gopls 的程序建立 sibling/PATH/symlink/hardlink 布局，无参数经过生产入口，覆盖输入刷新、合法升级、固定路径损坏/修复与自身身份变化。路径与权限失败的组件断言明确使用注入系统调用错误，不把 root 下未触发的 chmod 错误当作证据。

`integration/default_launcher*_test.go` 对应必跑 raw suite；`TestRealGoplsDefaultLauncherTwoProtobufUpgrades` 是独立的真实后端默认入口，原显式测试仍保留。`scripts/check-launcher-tests.py` 从 JSON 确认每个新增入口用例实际运行并通过，缺失、失败或 skip 均使对应 CI 步骤失败。

平台、提交 tree、工作树、Go 版本、run/attempt 和权限身份写入 `source.json`；安装路径、argv、PID、二进制哈希及原始轨迹归档在监测树外。当前结果与 36 项用例映射见[默认入口实施记录](changes/default-gopls-launcher/implementation-results.md)。真实 gopls 不代替独立刷新因果，raw harness 不代替真实 Codex。

## 每次工具调用重建

`integration/per_call_test.go` 用只在启动时读取文件的夹具，覆盖无文件变化也换进程、监测范围外和排除文件的更新、现代首调用、串行排队与反向响应/进度/控制通知、取消、不重放和 batch 拒绝。`internal/proxy/per_call_test.go` 补充配置校验、响应交付前不换代、等待期间继续轮询、取消队首不转让新进程及 typed ID 边界。

`TestRealGoplsPerCallFreshProcessAndFileChanges` 使用固定 v0.24.0 的 sibling gopls，关闭 watcher 并将轮询设为 1h；七次 `go_search` 分别验证不变输入、新增、重命名、同 mtime 内容修改、旧符号消失与删除，每次记录新 boot/PID/generation，并核对原生工具目录保持不变。该用例验证实际 gopls 互通，不能替代上述独立夹具的刷新因果证据，也不证明真实 Codex 客户端验收。

`scripts/check-launcher-tests.py` 同时检查上述三个 `per_call*_test.go` 文件中的测试：对应组件、raw 和真实后端步骤必须实际执行并通过，缺失、失败或 skip 均拒绝通过。该模式仍保留原有输入校验，不承诺未保存编辑、并发写入的一致快照或跨进程业务 handle 的迁移。
