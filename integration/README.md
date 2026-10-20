# 原始协议与进程集成验收

`raw_proxy_test.go` 通过实际 CLI 子进程、独立换行 JSON 客户端和
`internal/testbackend` 验证代理行为。夹具不导入生产 `wire` / `protocol`，也不使用
MCP SDK。每次启动只读取一次输入清单，之后查询仅返回内存快照；输入读取次数、
随机 boot ID、真实 PID、原始接收帧和业务执行记录写到监测树之外。

先用 `TestRawFixtureNeverRefreshesItself` 自证：同进程在磁盘 A→B 后仍返回 A，
新进程才返回 B。代理刷新测试随后关联同一 proxy PID、不同 helper boot/PID、
新快照和独立执行记录。默认关闭 watcher、把轮询间隔设为 1h，确保大部分刷新
用例必须由派发前内容校验发现变化；轮询和 watcher 各有独立用例。

运行：

```sh
go test -race ./integration -run TestRaw -count=1 -v
GOPLS_INTEGRATION=1 GOPLS_BIN=/absolute/path/to/gopls \
  go test -race ./integration -run TestRealGoplsTwoProtobufUpgrades -count=1 -v
```

`-race` 构建标记同时用于实际 proxy 二进制，避免仅检测测试客户端。
夹具使用 `GORACE=atexit_sleep_ms=0`，保留竞争检测并避免每次替换附加 1s 退出等待。
测试清理检查子进程 stderr 中的竞争报告。设置 `PROXY_TEST_EVIDENCE_DIR` 后，
测试会保留每例的独立原始双向 transcript、执行记录和 proxy stderr，供 CI 上传。
100 轮刷新与固定 seed 混合故障用例在普通 CI 执行，
`-short` 时明确跳过；必需夹具其余用例不因缺少 gopls 而跳过。

## 当前用例覆盖

下表是实际断言范围，不将一个通过的测试扩展成整个设计 VAL 已完成。

| 用例 | 对应设计编号 | 实际断言 |
|---|---|---|
| RawFixtureNeverRefreshesItself | FIX-001 | 同 PID 不自刷新，读取次数固定，新 PID 才读取新内容 |
| RawProxyRefreshEachRevision | INPUT-001/002/005/007/011、GEN-001、MCP-001/005 子断言 | 四个 legacy 与 2026-07-28 分别测试 `.go` 和四类依赖文件连续两次变化；等长并恢复 mtime；现代无 initialize；任意依赖字节可通过 |
| RawProxyPayloadIdentity、FrameLimitTerminatesWithoutPartialDispatch | FIX-002、RAW-001/002/003/005、ID-001 子断言 | 未知正文、顶层字段、metadata、复杂 schema、上游错误、工具错误、大整数和字符串/数字 ID 原样；262KB 内容完整通过；超限帧关闭且不部分派发 |
| RawProxyArgvAndEnvironment、ExternalWatchRootKeepsCWD | CLI-001/002/003、INPUT-006 子断言 | 空 argv、Unicode、空格、字面 shell 字符及后端 help/version 保留；额外监测根不改变 cwd/env |
| RawProxyNeverReplaysDispatchedBusiness、DispatchedBatchNeverReplayed | NOREPLAY-001/002/004、RAW-004 子断言 | 已记录执行的阻塞、崩溃、半帧退出均不重放；副作用诊断如实反映派发状态；已写 batch 崩溃后所有请求成员不重放 |
| RawProxyWriteToolReportsSideEffects | ERROR-003 | 写入已执行、响应已收到，输入变化后明确失败；不提供安全重试承诺 |
| RawProxyReverseRootsAcrossRefresh | MCP-002、ID-002 子断言 | roots 来自真实客户端；不同后端重复的 reverse ID 映射到不同前端 ID |
| RawProxyLegacyBatch、AllQueuedBatchMembersCancelled | MCP-004、CANCEL-001 子断言 | 2025-03-26 混合请求/通知 batch 保留分组，全部排队成员取消后不发空数组、不执行 |
| RawProxyConfirmedLegacyStateRecovery | STATE-001/007 子断言 | 已确认订阅与日志级别恢复；已确认退订不复活 |
| RawProxyModernMetadataAndIDReuse | MCP-005/006、ID-003 子断言 | 首条普通请求、不插入 discovery、逐请求 metadata 原样、已完成 ID 合法复用 |
| RawProxyModernMRTRAcrossRefresh | STATE-002/003 | token 不透明传递；换代后后端真实拒绝原样返回；只有客户端发出两轮调用 |
| RawProxyModernSubscriptionRefresh、ModernMultipleSubscriptions | STATE-004 子断言 | ack 后仍长期存在；多订阅按 ID 路由、独立取消、completion；刷新有界结束且不自动重订 |
| RawProxyIdlePollRefresh | INPUT-008 子断言 | 无业务请求且 watcher 禁用时轮询主动换代 |
| RawProxyWatcherSameContentInvalidates | INPUT-004 子断言 | 已观察同内容写入也使旧进程失效 |
| RawProxyModernCancelAndReuseID、CancelQueuedRequest | CANCEL-001/003、ID-003 子断言 | 未发送取消不执行；已发送取消不额外返回响应；歧义 ID 复用先隔离旧进程 |
| RawProxyUnknownInitializeRequiresReconnect、UnknownRevisionReconnect | STATE-006、MCP-009 子断言 | 未知初始化字段/修订稳定期原样，缺少恢复规则时关闭连接 |
| RawProxyDiscoveryPreservesDifferentErrors | MCP-007 子断言 | discovery 的 -32601、-32042 与 1234 保持原样，不只处理一个旧版错误码 |
| RawProxyHundredRefreshes | LIFE-004 子断言 | 固定顺序 100 轮内容替换，101 独立 helper boot，同一 proxy PID |
| RawProxyHundredMixedFaults | LIFE-004、NOREPLAY、CANCEL、ID-003 子断言 | seed 703031 的 100 次写入/正常 ID 复用/已派发取消/崩溃/半帧退出；独立执行计数至多一次，退出后所有记录的 helper PID 均已消失 |
| RealGoplsTwoProtobufUpgrades | GOPLS-001/002、GOPLS-003 子断言 | 原生工具目录直连对照；protobuf 1.34.2→1.35.2→1.36.6；源码编辑、新增、删除、原子替换均增加发布代次 |

## 证据边界

真实 gopls 可自刷新，所以其结果只作为互通证据；独立刷新因果来自无自刷新夹具。
真实互通用例不声称比较了所有原生方法，只完整比较稳定 `tools/list`，并执行
`go_file_context` / `go_search`。专用 CI 必须启用 `GOPLS_INTEGRATION=1`，缺少
二进制或依赖下载失败会失败，而不是静默跳过。

上述集成用例尚不单独证明所有扫描/原子发布调度交错、所有 discovery/fallback
分支、sampling/elicitation、所有进度 token 竞态、batch 部分写入注入、未知目录
分页组合、所有进程后代/背压组合和性能分位数。
其中帧短写、扫描根/符号链接、协议字段、目录恢复与进程生命周期还有对应模块
单元测试；完整设计门禁仍需结合实际运行结果逐项判断。

## 默认启动入口的增量测试

`default_launcher_test.go` 与 `default_launcher_boundary_test.go` 使用编译后的 proxy 和
`testdata/default-gopls` 独立程序。该程序只接受 `[mcp]`，复用 stdlib 实现的只读一次夹具，
没有给生产 resolver 增加环境覆盖或测试开关。真正零参数用例保持全部生产默认值；
带 watch-root/include/exclude/GoDiscovery 的用例明确属于仅代理选项模式。

覆盖实际安装布局、原始协议字段、五类输入连续两次等长同 mtime 修改、已派发阻塞请求不重放、
固定 PATH locator、原路径文件/symlink 升级、坏路径修复、默认恢复窗口耗尽、代理启动与当前
两份身份，以及 home/root 和本地诊断边界。直接 fixture 的负向对照先证明后端不自刷新。

```sh
go test -race -json ./integration -run '^TestRawDefaultLauncher' -count=1 > /tmp/default-launcher.json
python3 scripts/check-launcher-tests.py raw /tmp/default-launcher.json
GOPLS_INTEGRATION=1 GOPLS_BIN=/absolute/path/to/gopls \
  go test -json ./integration -run '^TestRealGopls' -count=1 > /tmp/real-gopls.json
python3 scripts/check-launcher-tests.py real /tmp/real-gopls.json
```

真实默认入口将固定 v0.24.0 gopls 与 proxy 放在 sibling 布局，运行时不传入后端路径或代理参数。
保留原显式互通测试；两者均比较完整工具目录并运行两次 protobuf 升级及四种源码变化。
默认入口还保留 application-exchanges.jsonl（应用请求/响应，不声称含所有反向请求）。
实际执行状态、Codex 客户端结果和剩余限制见[实施记录](../docs/changes/default-gopls-launcher/implementation-results.md)。
