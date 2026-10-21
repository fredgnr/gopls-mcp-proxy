# 空 extensions 初始化恢复修复

## 根因与边界

2026-10-21 的独立 stdio 复现报告使用 `v0.5.0` / `0ebaa30c988256ac08c2e1d4cfe9a3a7c85e5c83`。在保留 `elicitation.form/url` 和 `experimental["codex/auth-change"]` 空标记的情况下，只增加 `capabilities.extensions: {}`，便会使 `per-call` 初始化失败。

根因在 `internal/proxy/recovery.go` 的 `recoveryCapabilityShape`：恢复白名单没有 `extensions` 规则，递归诊断因此在 `params.capabilities.extensions` 返回 `no_recovery_rule`。后端已经成功回复，但代理判断初始化无法恢复，于是返回 `protocol_restart_required` 并关闭连接。

本次为协商版本 `2025-06-18`、`2025-11-25` 的客户端添加空映射兼容规则。空映射不包含任何具体扩展声明，不引入扩展状态；代理保留它在原始初始化和后续恢复请求中的内容，不删字段、不升级协议。这是明确限定范围的兼容规则，不声称所有协议版本都将该字段定义为标准能力。

| 声明 | 恢复行为 |
| --- | --- |
| 客户端省略 `extensions` | 保持原有行为，恢复时仍省略 |
| 上述两个协商版本的客户端 `extensions: {}` | 允许恢复，原样保留 |
| 非空 `extensions`，包括值为 `{}` 的扩展键 | 拒绝；当前没有实现任何具体非空扩展的恢复语义 |
| `null`、数组、字符串、布尔值、数字 | 拒绝，诊断为 `invalid_type` |
| 服务端 `extensions`、其他协商版本 | 保持拒绝；本次未扩大范围 |

`experimental["codex/auth-change"]` 的既有规则继续生效，不能把该键放到 `extensions` 中借用恢复许可。未知扩展、初始化信息变化、无法恢复的会话状态仍按原有逻辑拒绝。

## 回归覆盖

- 单元测试检查省略与空对象、协商版本、重复恢复时参数保留，以及非空声明和全部 JSON 错误类型的拒绝路径。
- Raw 后端测试覆盖报告中只增加/删除 `extensions` 的对照组合，在 `per-call` 和 `on-change` 下检查初始化保留、后端代次、旧进程退出、无工具自动重放及脱敏错误。
- 真实 gopls 测试使用单独的报告 fixture，连续运行 `go_workspace`、`go_file_context`、`go_diagnostics`，并验证后续文件增删改触发的恢复。既有 Codex 源码重建 fixture 保留，继续回归此前的 auth-change 修复。
- CI 保留 Linux / macOS、gopls `v0.23.0` / `v0.24.0`、race、最低 Go 版本和四平台构建门禁。

报告 fixture 来自独立客户端的复现请求，不能据此断言用户 Mac 上真实 Codex 的 `extensions` 也为空。实际 Codex 连接的验收结果单独记录，模拟客户端和真实 gopls 测试不冒充真实 Codex 验收。

本地对照已确认：基线提交的空对象用例在 `per-call` 初始化阶段失败，在 `on-change` 文件变化后失败，均为报告中的 `no_recovery_rule`；省略字段的对照通过。修复后的新旧 fixture 在真实 gopls `v0.24.0` 下通过两种模式，每组完成 13 次工具调用；`per-call` 发布 14 代后端，`on-change` 发布 8 代后端，旧进程退出检查全部通过。

合并只更新仓库代码，不会替换已安装的 `v0.5.0` 二进制。使用修复需要安装新构建并建立新的 MCP 连接。
