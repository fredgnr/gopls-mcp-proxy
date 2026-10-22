# PR #8：两项代码问题修复后的验证证据

本目录保存修复“取消首个未派发请求后无法重新唤醒”和“URL elicitation 未完成时错误释放”之后的本地验收记录。数据采集自已推送且干净的源提交，原始测试文件和日志均未改写。

## 来源和二进制身份

- 源提交：`4a3a95f98230f4ced6a13e973f1d89b6715220b2`。
- tree：`88181eb03dc9b04b947439547e0b0065387bcede`；测试前 `git status --porcelain` 为空，见包内 `pr8-fixed/source.json`。
- Go：`go version go1.27.2 linux/amd64`，测试二进制报告 `test_go_version=go1.27.2`。
- proxy SHA-256：`2752e2ac43a19af111a2b942dd19b45c0562601f937406472df79e37f0ddc36d`；Go build info 独立记录 `vcs.revision=4a3a95f98230f4ced6a13e973f1d89b6715220b2`、`vcs.modified=false`。
- proxy CLI 自报 `gopls-mcp-proxy dev (commit unknown)`，保留测试构建原值；源提交身份来自 Go build info，不能把 CLI 字段改写为发行版。
- gopls：`golang.org/x/tools/gopls v0.24.0`，SHA-256 `dfb525874833cb1fd6f8a72c14e5d711c9958197b1983407fce32e871b54984f`，使用 Go 1.27.2 构建。
- 三份 summary 均保存完整 `proxy_build_info` 和 `gopls_build_info`。

## 验证结果

- `go test -race ./cmd/... ./internal/... -count=1`：通过；包含两项问题的定向回归、交付/writer ACK/空闲边界、同一前端连接上的新 boot 后继请求和未知状态保护。
- 真实后端测试：resident、on-demand 各 100 次调用，另执行主进程强杀与恢复；总耗时 `193.293` 秒，通过。
- 严格组件及真实生命周期门禁：通过，见本目录 `unit-gate.json`、`real-gate.json`。所有指定用例实际执行，没有 test skip 或失败。

| 场景 | 工具调用 | 主后端/telemetry | 最大 telemetry 消失滞后 | 休眠 FD |
|---|---:|---:|---:|---|
| resident | 100 | 101/101 | 61 ms | 不适用 |
| on-demand | 100 | 102/102 | 61 ms | 100 次，值 [6] |
| 主进程强杀与恢复 | 3 | 5/5 | 20 ms | 3 次，值 [6] |

滞后按每个 telemetry 的 `first_observed_absent_ms` 减所属父进程的 `first_observed_exit_ms` 计算，门禁为 6000 ms 加 100 ms 采样误差。三个场景的全部已归属进程均有退出和消失时间，观察器没有错误。逐代 PID、启动身份、父子归属、PGID/SID 和完整采样在原始 `process-samples.json`。休眠不因文件变化启动、ping 唤醒及每次工具使用新 boot 均由此源提交中的真实测试断言检查。

## 文件与复核

- `fixed-local100-evidence.zip`：源身份、完整组件测试 JSON、真实测试 JSON、三个场景全部 summary/进程采样/proxy 日志、原门禁输出，以及单独标记的修复前复现日志。
- `manifest.json`：每个原始文件的 SHA-256、精确源和二进制身份、统计结果、门禁来源。
- `unit-gate.json`、`real-gate.json`：与包内文件一致的门禁输出，便于直接阅读。
- `SHA256SUMS`：本目录文件校验值。

```sh
sha256sum -c SHA256SUMS
unzip fixed-local100-evidence.zip -d fixed
# 在源提交 4a3a95f98230f4ced6a13e973f1d89b6715220b2 的 checkout 中执行：
python3 scripts/check-lifecycle-tests.py unit fixed/pr8-fixed/unit-race.json
python3 scripts/check-lifecycle-tests.py real fixed/pr8-fixed/real.json fixed/pr8-fixed/real
```

包内 `auxiliary-pre-fix-reproductions/` 是在旧实现上新增确定性测试后取得的失败日志，**不是原始干净 HEAD 的运行**；这两次复现没有单独保存完整 worktree/tree/工具链元数据，因此只用于说明缺陷，不能代替本次修复提交的验收。

[对应 GitHub CI](https://github.com/fredgnr/gopls-mcp-proxy/actions/runs/39748428008) 已全部通过，覆盖 Linux/macOS × gopls v0.23.0/v0.24.0、Go 1.25 兼容性、组件/raw race、真实后端门禁及构建。运行 attempt、任务结果和 artifact 索引见 `ci-status.json`。本地 100 次完整原始记录不依赖 CI artifact 保留期限。

这些是合成单会话、非 race 的正确性/稳定性数据，不构成生产内存收益结论，也没有执行用户实际 Mac/Codex 连续调用。后者在修改宿主配置并正式启用 on-demand 前另行完成。
