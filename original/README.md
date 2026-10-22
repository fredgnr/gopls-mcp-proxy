# PR #8：原始本地 100 次循环证据

这是此前 PR 描述所指的原始测试数据，现补充公开索引。原始文件没有改写；本索引和门禁输出在 review 修复期间生成。

## 来源和身份

- 实测源提交：`1f3d8558c8a2396f84ed6d87605628145d579171`。
- 实测 tree：`240010a9d72c92cb3613808fe4818e1144e30ed2`；当时 `git status --porcelain` 为空，见包内 `source-final.json`。
- 原始测试 JSON 记录运行于 2026-10-22；总耗时 255.006 秒。
- proxy SHA-256：`e1f62a8fa87a1ecb8b932b29ebfada10d3f18d24fc5b42ba15c95cad5938d351`。该测试构建自报 `version=dev, commit=unknown`，不能把此值改写为发行版本。临时二进制已被测试清理，其完整 Go build info 当时未保存。
- gopls：`v0.24.0`，SHA-256 `dfb525874833cb1fd6f8a72c14e5d711c9958197b1983407fce32e871b54984f`；各 `lifecycle-summary.json` 保存完整 gopls build info（`go1.27.2`）。
- 原执行命令通过 PATH 指定 Go 1.27.2；未单独保留同时刻 `go version` 输出。此说明来自执行记录，不伪装为原始日志字段。

## 已记录结果

| 场景 | 工具调用 | 主后端/telemetry | 最大 telemetry 消失滞后 | 休眠 FD |
|---|---:|---:|---:|---|
| resident | 100 | 101/101 | 161 ms | 不适用 |
| on-demand | 100 | 102/102 | 587 ms | 100 次均为 6 |
| 主进程强杀与恢复 | 3 | 5/5 | 40 ms | 3 次均为 6 |

滞后按每个 telemetry 的 `first_observed_absent_ms` 减所属父进程的 `first_observed_exit_ms` 计算，门禁为 6000 ms 加 100 ms 采样误差。每种场景的所有已归属进程均有退出和消失记录，观察器无错误。具体 PID、出生身份、父子归属、PGID/SID、采样及退出时间在原始 `process-samples.json`。

## 文件与复核

- `prior-local100-original.zip`：11 个未改写原始文件；包含测试 JSON、源身份，以及三个场景的 summary、进程采样、proxy 日志。
- `manifest.json`：每个文件大小和 SHA-256、场景统计、版本身份、证据局限。
- `strict-gate-regenerated.json`：review 时使用实测提交中的原始检查脚本与测试名来源，对原数据重新检查所得 PASS。原执行记录中有门禁 PASS，但未保留为单独文件，故这里明确标为重新生成。
- `SHA256SUMS`：本目录四个数据文件的校验值。

```sh
sha256sum -c SHA256SUMS
unzip prior-local100-original.zip -d original
# 在实测提交 1f3d855 的干净 checkout 中执行：
python3 scripts/check-lifecycle-tests.py real \
  original/real-soak-v024-verified.json original/real-soak-v024-verified
```

该次数据只验证当时的产品代码；后来只有测试工具变更的 `1dd8743` 没有改变产品实现，但本次两个产品问题修复后的验证应以新记录为准。数据属于合成单会话正确性/稳定性采样，不是生产内存收益实验，也没有执行实际 Mac/Codex 宿主验收。
