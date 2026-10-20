# Codex auth-change 初始化恢复修复

## 故障与复现

2026-10-20 的报告定位到 `v0.4.0` / `52e9845d5e04654a45bfca8d36ba648c36a1833c`：初始化成功，但 `per-call` 第一次工具调用触发后端替换时，恢复校验拒绝 `params.capabilities.experimental["codex/auth-change"]`，随后关闭 stdio。后续 `Transport closed` 是关闭连接的结果。

该提交把 `experimental` 的恢复形状定义为空对象。只要它含有 `codex/auth-change` 这个键，即使值是 `{}`，也会得到报告中的 `nonempty_static_declaration` / `actual_type=object`。因此日志不能证明内层对象非空；修复前补入上游实际声明即可使 `TestRecoveryCodexInitializationPreservesKnownCapabilities` 复现完全相同的错误。

旧 Codex fixture 仅包含 `elicitation.form/url`，漏掉了有认证状态 watcher 时添加的实验能力。此前测试并未覆盖这条启动路径。

## 上游语义与兼容边界

依据 Codex `rust-v0.163.0` 的精确提交 `83086188ae0614bd882818ff1aa23f3194962012`：

- [`rmcp_client.rs`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/codex-mcp/src/rmcp_client.rs) 在存在认证 watcher 时，向客户端 `experimental` 映射加入 `"codex/auth-change": {}`。
- [`auth_changes.rs`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/codex-mcp/src/auth_changes.rs) 仅在服务端也声明该能力时发送 `notifications/codex/authChanged`；先发送当前 `generation`、`ownerGeneration`，再监听变化。

本次仅为客户端的空标记建立恢复规则。保存和重放原始声明，不删除扩展、不改写身份、不合成认证通知。服务端同名能力继续拒绝恢复，因为启用后还需要重建认证通知状态与处理刷新期间的通知顺序。非空内部对象、错误类型以及其他未知实验能力仍拒绝恢复。

`per-call` 模式在初始成功响应的内容新鲜度检查后检查恢复资格；无法恢复时返回有字段路径的 `protocol_restart_required`，并有界关闭连接。此时初始化请求已经派发且后端已回复，错误正确标记 `requestDispatched=true`、`backendResponseReceived=true`、`automaticReplay=false`。不会把初始化错误伪称为未执行。后端原生错误不变。`on-change` 继续允许首次透传，在需要刷新时检查恢复资格。

## 验证

测试分三层：

1. `internal/proxy`：已知声明与版本协商、重复恢复时原始参数保留；非空值和错误类型、未知实验项及服务端 opt-in 拒绝；诊断不输出字段值；`per-call` 初始化提前失败及有界关闭。
2. Raw fixture：同一前端在 `per-call` / `on-change` 下恢复，完整初始化参数不变、旧进程退出、工具调用不自动重放；非法声明保持明确错误与脱敏诊断。
3. 真实 gopls：连续调用 `go_workspace`、`go_file_context`、`go_diagnostics`，再验证文件增删改与诊断变化，检查后端代次、PID 和退出情况。

现有 CI 在 Linux / macOS 上运行 gopls `v0.23.0` / `v0.24.0` 真实后端矩阵，同时执行 race 测试与发行包构建。没有修改工作流或降低门禁。

可在安装对应 Go 与 gopls 后执行：

```bash
go vet ./...
go test -race ./cmd/... ./internal/... -count=1
go test -race ./integration -run '^TestRaw' -count=1
GOPLS_INTEGRATION=1 GOPLS_EXPECTED_VERSION=v0.24.0 \
  go test ./integration -run '^TestRealGopls' -count=1
```

fixture 根据官方源码重建，不是原始故障连接的完整抓包。真实后端测试由 raw MCP 客户端驱动，不等同于用户 Mac 上运行 Codex 的现场验收。合并代码不会替换用户已有的 `v0.4.0` 二进制；使用修复需要重新安装并建立新的 MCP 连接。
