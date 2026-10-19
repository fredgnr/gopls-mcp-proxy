# 默认 gopls 启动入口：Codex 只修改 command

状态：**Proposed**。日期：2026-10-19。本次提交只包含技术方案；新的默认启动入口、运行时测试及 Codex 验收均为 **pending**。

实现基线为 [dc9821f76228985333aa590bb1705149d1ba2061](https://github.com/fredgnr/gopls-mcp-proxy/commit/dc9821f76228985333aa590bb1705149d1ba2061)，tree 为 314229ab52794775e5389febf29d15ef910455a8。该基线已经实现 proxy 独立刷新与原始 MCP 中继，但 CLI 仍要求显式指定后端。

## 目标体验

用户已经安装原生 gopls 和代理，Codex 使用以下配置：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls-mcp-proxy"
```

相对于题述配置，只把 command 从原生 gopls 路径改成代理路径；该 MCP 条目仍只显式设置 command 和 default_tools_approval_mode，不要求增加 args、env、cwd 或超时配置。代理选择同目录的 gopls，必要时从继承的 PATH 查找，默认启动 `gopls mcp`。原生 gopls 文件应保留。

这是本方案实施后的目标配置，**当前基线无参数启动仍会报错**。当前可用的显式形式见[现有运行说明](../../../README.md)。

## 适用条件与边界

- Codex 实际以目标项目目录作为进程 cwd；安装目录与项目目录分开。
- 要求代理感知变化的输入处于默认监测树内。默认不会自动增加 cwd 外的本地 replace、其他仓库或 Go 工作区目录。
- 原生 gopls 可运行，其所需的 Go 工具链与环境已经存在。
- 在固定 Codex / gopls 版本和验收项目规模下，启动与工具调用满足客户端默认预算。
- 本轮是 macOS / Linux、MCP stdio、单客户端、单活跃后端；运行时仍沿用既有协议恢复和一致性边界。

官方原生 gopls 的 MCP stdio 入口是 `gopls mcp`；裸 `gopls` 默认运行 LSP。题述配置本身能否在用户机器上提供 MCP，取决于实际二进制和完整生效配置，不能预设成功。因此验收同时使用正确的原生 MCP 对照，以及只显式设置上述两项的目标 MCP 条目。[上游依据](design.md#2-事实基线与来源)

已经配置了 `args = ["mcp"]` 的标准原生配置不属于本轮“只改 command”的承诺。它迁移到默认模式时应删除该 args；也可以继续使用显式 `-- program argv` 模式。本轮不把所有 gopls CLI 参数都解释为代理参数。

## 阅读顺序

| 文档 | 内容 |
|---|---|
| [技术设计](design.md) | CLI 模式、默认后端定位、自身身份检查、工作目录、预算和兼容边界 |
| [实施计划](implementation-plan.md) | 分阶段任务、依赖、文件归属、子代理协作与完成条件 |
| [验收计划](validation.md) | 参数和路径用例、无自刷新后端、真实 gopls、真实 Codex 与证据规则 |

## 主要决策

1. 默认模式提供 `gopls` + `["mcp"]` 的启动约定；显式模式继续包装任意 MCP stdio 程序。
2. 同目录以实际代理可执行文件所在的物理目录为准；默认 PATH 搜索只考虑绝对目录。
3. 候选确实不存在时才继续搜索；已存在但损坏、不可执行或指向自身时明确失败。
4. 一次代理运行只选择一次后端定位路径；每次启动重新检查文件身份，保留该路径接受正常二进制升级的能力。
5. 不改调用方 cwd、环境、监测默认范围、Go discovery 默认值或超时上限，也不新增工具业务适配。
6. 真实 Codex 验收与 raw harness 结果分别记录。缺少真实客户端或平台证据时保留 pending / blocked，不宣称一行替换已经通过。

## 与既有方案的关系

[独立刷新与协议兼容方案](../proxy-refresh-protocol/README.md)及[当前运行设计](../../design.md)仍是协议与刷新核心的基线。本提案只计划扩展“必须显式提供后端”的 CLI 条件，并增加启动前的文件身份检查。历史实施记录和通过结果保留原含义。

实施完成后，应更新当前 README、运行设计、验证范围及打包说明，并登记实际验证提交。文档 PR、既有 CI 或交叉编译成功，都不代表默认入口和真实 Codex 验收已完成。
