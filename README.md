# PR #8 生命周期验证证据

此分支只存放验证记录，不用于构建或合入产品分支。

- [修复后验证](fixed/README.md)：源提交 `4a3a95f98230f4ced6a13e973f1d89b6715220b2`；干净 worktree、Go/proxy/gopls 身份、组件 race、两种模式各 100 次工具调用、故障恢复和严格证据门禁全部通过。附完整原始记录、ZIP、manifest 和 SHA-256。
- [原始本地 100 次循环](original/README.md)：源提交 `1f3d8558c8a2396f84ed6d87605628145d579171`；保留未改写的历史数据及记录局限。

[修复后 GitHub CI](https://github.com/fredgnr/gopls-mcp-proxy/actions/runs/39748428008) 的实时状态以 run 为准。实际 Mac/Codex 宿主验证尚未执行，在正式启用 on-demand 前完成；合成单会话数据不作为生产内存收益结论。
