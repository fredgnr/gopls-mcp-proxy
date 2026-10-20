# 默认启动入口实施记录

日期：2026-10-20。实现基于方案分支 `50aaf08f7b1f0465e316c19721917b0b705b0bdf`，其 tree `4c2be439fadea58f37896790d83a11daf90eb810` 已逐文件与远端核对。协议/刷新核心基线为 main `dc9821f76228985333aa590bb1705149d1ba2061`。

代码交付与真实客户端验收分别记录。最终远端提交、tree、Actions run/attempt 和平台结果以 [PR #2](https://github.com/fredgnr/gopls-mcp-proxy/pull/2) 的提交及检查为准；CI 的 `source.json` 将每次结果绑定到实际 checkout（PR 检查可能是合成 merge commit），不把本文件的祖先版本当作被测实现。

## 实现范围

- CLI 无参数或只有合法代理选项时选择默认 gopls，后端 argv 恰为 `[mcp]`。显式 `-- program argv`、原参数克隆与原解析语义保留。
- `internal/launcher` 从代理物理目录 sibling、继承 PATH 的绝对目录顺序选择。只有 Lstat 真正不存在才继续，已存在但损坏的候选明确失败。
- 保存后端绝对 locator 和代理启动文件身份。生产 CLI 两种模式都注入逐次检查，比较启动 inode 与当前自身 locator；拒绝自身别名和未知身份。
- `Factory.BeforeStart` 在实际执行前检查，失败消耗 boot ID、关闭所有管道，不产生新进程；保持原路径升级和既有有界恢复。
- cwd、env、监测默认范围、Go discovery、协议正文、请求不重放以及超时默认值保持原契约。

独立审查由未编写相应模块的协作角色进行：launcher/CLI/backend 的职责、失败边界和默认入口夹具通过审查；审查发现的覆盖缺口（初始自身拒绝、恢复期限、选项效果、home/root）已补入测试。没有生产测试开关或后端路径环境覆盖。

## 本地执行

本地为 Linux amd64，Go **1.26.1**、gopls **v0.24.0**；Go 版本与规定 CI 矩阵不同，因此本地通过不替代 Go 1.25.x/1.27.2 或 macOS 结果。测试日志、缓存和证据都在被监测项目之外。

| 检查 | 结果与范围 |
|---|---|
| `go test -race -json ./cmd/... ./internal/... -count=1` | pass，104 个顶层组件测试；默认解析、身份、权限错误注入、backend、workspace、wire/protocol 全部执行 |
| `go test -race -json ./integration -run '^TestRaw' -count=1` | pass，39 个顶层 raw 测试、0 skip；包含全部旧套件与 12 个新增默认入口测试，耗时 105.448s |
| `GOPLS_INTEGRATION=1 … go test -json ./integration -run '^TestRealGopls' -count=1` | pass，原显式与新默认两个用例；最终默认选路断言另跑通过（30.40s） |
| `go vet ./...`、gofmt、`git diff --check`、actionlint | pass |
| 默认恢复期限 | 实际零参数进程实测约 60.004s；无后备程序启动、旧结果成功或遗留后端 |
| 四目标打包 | 交由最终提交的既有 CI 构建；本地交叉编译不替代平台运行 |

真实 gopls 的默认入口使用真实构建文件作为 sibling，整个运行无代理参数、无运行时 GOPLS_BIN 覆盖。完整工具目录与独立 `gopls mcp` 对照一致；protobuf `1.34.2 → 1.35.2 → 1.36.6` 和源码编辑/新增/删除/原子替换在同一代理 PID、前端连接内产生七次后端发布。互通证据不承担独立刷新因果证明。

对应本地非 race 真实 gopls 测试的 proxy SHA-256 为 `60633a7c6ad0a509c4ccce0dde49877086e61cfbfc80387a2f16ad8446d09db3`，gopls 为 `5177cceba7df05be62efa67ff22ba22a95e7182fcb524b450e2159efe2d9a71c`。CI 不要求跨工具链二进制哈希相同，会保存其自身构建的哈希。

## 用例状态

以下是本地断言范围。001～032 的平台推广仍受 036 约束；本地完整 raw 回归已通过；CI 尚未完成时不提前声称最终交付矩阵通过。

| 用例 | 证据/断言 | 本地状态 |
|---|---|---|
| 001～005 | `TestDefaultGoplsIntent`、`TestBackendArgumentBoundary`、原 CLI 错误/迁移/参数测试；仅替换 nil 与合法 options 无后端的旧失败断言 | pass |
| 006 | setup 计数、删除 cwd 的组件用例及真实二进制 stdout/stderr/无 Go 探针 | pass |
| 007～015 | launcher resolver/identity 测试，实际物理 symlink/hardlink 布局；EACCES 为明确注入系统调用错误，不冒充 root chmod 实验 | pass |
| 016～018 | 生产默认 sibling/PATH/显式初始自身拒绝；真实运行文件替换后检查旧/新身份；独立 backend helper 及每次 attempt/管道清理 | pass |
| 019～022 | 固定 PATH locator、文件/symlink 升级、八类坏定位修复、默认恢复窗口耗尽；cfg/env/argv 快照由 backend 回归补充 | pass |
| 023～024 | 真构建文件、实际零 argv sibling/PATH，initialize、tools/list、tools/call、原始帧和真实 PID | pass |
| 025 | watch-root、include/exclude 的实际效果及显式 GoDiscovery 的 go.work use 范围；cwd/argv/env 保留 | pass |
| 026 | 同 fixture PID 的 A→B 仍返回 A、读取次数不变；新 PID 才返回 B | pass |
| 027 | `.go` 与四类依赖文件各连续两次等长同 mtime 变化；同 proxy、新 boot/PID、只读一次；已派发 hold 执行一次且不重放 | pass |
| 028～029 | cwd/env/roots、默认外部输入未覆盖、home/root 拒绝、stderr/原始消息；无 Go 调用 | pass |
| 030 | 全部原组件和旧 raw 套件完整回归通过，不删除或削弱旧协议断言 | pass |
| 031～032 | 固定 v0.24.0 原生对照和零参数互通，两个 protobuf 升级、四种源码变化、七个后端 boot | pass |
| 033～035 | 真实 Codex 配置加载、发现/调用/同会话刷新，以及默认预算冷暖样本 | 见下节，不能由 raw 结果推定 |
| 036 | Linux Go 1.25.x、Linux/macOS Go 1.27.2；真实行固定 gopls v0.24.0；JSON 防跳过、只读权限和证据上传 | CI 已接入，等待最终远端执行 |

用例编号均为 `VAL-LAUNCH-xxx`。完整原始契约仍见[验收计划](validation.md)，这里不扩大组件断言或承诺未测试组合。

## 真实 Codex

真实 Codex CLI **0.159.2** 在 Linux amd64 上通过原生 app-server `mcpServerStatus/list` 分别连接合法原生控制和零参数 proxy，均发现相同的 **8 个工具**。`config/read` 核实目标只有 command/default_tools_approval_mode 两个显式配置来源，没有 args/env/cwd/timeout 覆盖；仅在该次调用中禁用了一个无关 MCP 服务，未修改日常配置或认证。

发现耗时为原生 **182.796ms**、代理 **104.700ms**，使用已有缓存、仅两个文件的小项目（156 bytes），单次数据不用于性能比较。代理构建哈希与上节真实 gopls 测试一致。脱敏事实和运行源码哈希保存在 [codex-local-summary.json](codex-local-summary.json)。

**033～035 未通过完整验收**：合法原生控制的 `thread/start(ephemeral=true)` 在 20s 及一次 45s 重试中均未返回，也没有可处理的待决客户端请求。阻塞发生在同 thread 工具调用之前，根因未确定；因此没有同一客户端 thread/proxy PID 下的工具调用、内容修改后调用或清理成功证据。各专用 app-server 测试进程均终止。

目标条目未增加 timeout，但该已安装客户端的 schema/config 元数据不暴露实际数值默认值，不能把设计中所列 10s/60s 当成此版本已测事实。没有冷缓存、工具调用或刷新计时样本。033 标记 blocked，034 只有真实配置与发现子断言 pass、完整用例 pending，035 blocked。该结果不妨碍提交已验证的入口实现，也不能写成“Codex 只改 command 已完整验收”。

## 任务状态

| 任务 | 状态 |
|---|---|
| TP-L0.1～L0.4 | completed：接口冻结、独立夹具自证、安装/证据布局及独立审查 |
| TP-L1.1～L1.3 | completed：CLI、resolver、双重自身身份组件 |
| TP-L2.1～L2.3 | completed：完整组件与 raw race 回归通过 |
| TP-L3.1～L3.3 | completed：实际默认入口、故障/刷新因果、真实 gopls |
| TP-L4.1～L4.2 | blocked/partial：真实配置与发现已验证；原生控制 thread/start 阻塞，完整调用/刷新和默认预算未验证 |
| TP-L4.3 | implemented：CI 选择器与证据接入，最终平台结果待运行 |
| TP-L4.4 | completed：README、使用边界、安装、设计及证据范围已同步 |
| TP-L4.5 | 源码/测试独立审查完成；最终提交和 CI 对应复核待远端结果 |

## CI 与证据复核

首轮实现提交 `b0ff06a3d8e86771bb20f52d5d7839424c3bf1ec` 的 [PR run 39364927320](https://github.com/fredgnr/gopls-mcp-proxy/actions/runs/39364927320) 与 [push run 39364923356](https://github.com/fredgnr/gopls-mcp-proxy/actions/runs/39364923356) 中，Linux Go 1.25.x/1.27.2 和四目标打包通过；macOS raw 的普通外部 watch-root 用例失败，未记为平台通过。

macOS artifact 显示 cwd 为 `/private/var/...`，测试额外根却保留 `/var/...` 别名。既有监测器按契约监听符号链接祖先，因而尝试监听 `/`，并记录 `/.VolumeIcon.icns` 不存在的注册错误；首次 initialize 随后因 filesystem_event 返回 workspace_changed。事件日志没有路径，不能断言具体是哪一个系统事件触发。修复只将这个普通外部目录夹具通过 EvalSymlinks 转为物理路径，保留真实外部根、全部 watcher/预算、生产核心和独立符号链接用例。修复后的最终提交与平台矩阵见 PR 检查。

`source.json` 保存实际 HEAD/tree、工作树、平台/权限身份、Go、run ID/attempt。各阶段 `unit.json`、`raw-contract.json`、`real-gopls.json` 保留实际执行情况；`launcher-{unit,raw,real}.json` 检查测试存在、已 run、已 pass，出现 skip/fail 或未执行立即失败。原始 fixture 事件、stderr、launch JSON、二进制哈希与真实应用消息保存在 runner 临时目录并统一上传。

使用原有 CI 矩阵和四目标 `release-dist` 构建；权限保持 `contents: read`、`persist-credentials: false`、固定 Actions SHA。不添加 `pull_request_target`、tag、Release 或发布动作。最终 PR 摘要登记提交/树和运行链接，使后续 HEAD 漂移时能明确区分证据对应版本。

文件检查与 exec 不是原子操作，检查不保证消除 TOCTOU 或包装脚本间接循环；固定 locator 的二进制升级仍需既有输入刷新触发。默认监测范围和客户端预算都不是对所有项目的无条件保证。
