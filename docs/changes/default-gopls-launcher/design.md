# 默认启动入口技术设计

状态：**默认入口及身份检查已实现**。实际执行结果与客户端边界见[实施记录](implementation-results.md)。阅读入口：[方案总览](README.md)；任务：[实施计划](implementation-plan.md)；证据：[验收计划](validation.md)。

## 1. 问题与目标契约

方案基线代理已经负责输入监测、后端换代和 MCP 原始消息中继，但启动方式强制要求 `-- program argv`，因此仅把 Codex 的 command 改成代理路径会在参数解析阶段失败。此次增加默认 gopls launcher，将后端选择和默认参数集中在启动层。

目标是在[总览](README.md#适用条件与边界)规定的安装、cwd、监测覆盖和客户端预算条件下，让没有 args 的 Codex 配置只修改 command 即可使用。它是一个可验收的配置体验，不是对所有机器、所有项目规模和所有原生 gopls 命令的无条件替代承诺。

| 编号 | 必须满足的要求 |
|---|---|
| DL-R01 | 无参数以及仅含合法代理选项时，可进入默认 gopls MCP 启动模式 |
| DL-R02 | 显式 `-- program argv` 保持原有通用行为；显式缺少后端仍失败 |
| DL-R03 | 默认定位顺序确定，不因候选损坏或每次重启而静默换用另一 gopls |
| DL-R04 | 拒绝已观察到的自身、符号链接自身、硬链接自身及后续重定向到自身的启动 |
| DL-R05 | 子进程 argv、cwd、环境及 MCP 字段保持对应模式的明确约定 |
| DL-R06 | 帮助、版本、错误和日志不污染 stdout；本地诊断不启动后端或扫描项目 |
| DL-R07 | 已有内容正确性、进程清理、协议恢复和禁止业务重放规则继续生效 |
| DL-R08 | 默认入口、真实 gopls 和真实 Codex 分别有证据，禁止用等价 harness 代替产品验收 |
| DL-R09 | 已有显式模式、符号链接升级、构建与平台回归继续通过 |
| DL-R10 | 文档、已安装二进制和验收提交可对应；不把旧 Release 或文档 CI 当作新增行为证据 |

## 2. 事实基线与来源

本轮读取的 main 为 `dc9821f76228985333aa590bb1705149d1ba2061`，tree 为 `314229ab52794775e5389febf29d15ef910455a8`。以下代码链接固定到该提交。

| 来源 | 已核实事实 |
|---|---|
| [CLI 与运行装配][S1]、[CLI 测试][S2] | 缺少分隔符或后端时报错；nil argv 当前被测试为非法；cwd/env 来自调用方 |
| [后端工厂][S3]、[后端测试][S4] | 固定绝对后端 locator，允许下次 spawn 读取升级后的映像；已有显式 argv/env/cwd 与进程清理测试 |
| [监测器][S5]、[当前 README][S6] | cwd 默认纳入扫描；home 与文件系统根被拒绝；Go discovery 默认关闭；额外输入需要明确范围 |
| [supervisor 默认配置][S7] | startup 45s、request 110s、refresh 60s、final cleanup 6s |
| [真实 gopls 测试][S8] | 对照是 `gopls mcp` 与显式 proxy 包装；不是无参数入口验收 |
| [Go 官方 MCP 文档][S9]、[gopls v0.24.0 命令分派][S10] | MCP stdio 使用 mcp 子命令；无子命令默认 serve |
| [Codex MCP 配置][S11]、[Codex 配置参考][S12] | command 指定程序；args/cwd 为独立选项；文档默认启动 10s、工具 60s |
| [v0.1.0 Release][S13] | 调研时仍指向改造前的 `32cbd9f`；新 main 与旧分发包不能混用验证结论 |

官方配置文档于 2026-10-19 核对。后续真实 Codex 验收仍需登记实际版本及生效值，不把网页默认值视作所有历史或未来客户端版本的保证。

## 3. 模块边界

建议新增小型 `internal/launcher` 包，负责默认启动约定、候选选择、身份检查策略和诊断信息。它不处理 MCP JSON、不解析 gopls 工具、不发现 Go 依赖，也不驱动刷新。

| 模块 | 此次职责 |
|---|---|
| CLI | 解析代理选项，选择 DefaultGopls 或 Explicit 模式，构造启动规格 |
| launcher | 默认 gopls 定位、自身身份快照、候选验证、启动检查策略 |
| backend.Factory | 保存定位路径/argv/cwd/env；在每次实际启动前调用已装配的检查，再执行原进程管理 |
| workspace / proxy / wire / protocol | 复用现有功能与默认值，通过回归证明装配没有破坏原契约 |

可以用 LaunchSpec 表达 `mode、locator、argv、selectionSource`，用 SelfIdentity 表达自身定位路径和启动时文件身份；名称为建议，接口以 L0 冻结结果为准。选择来源只用于 stderr 诊断，不能写入原生 serverInfo、工具 schema 或业务结果。

backend 的启动检查通过正常依赖注入装配，例如带 context 和 locator 的 BeforeStart 函数。生产 CLI 的默认模式与显式模式都装配自身检查；直接测试 backend 包时，可显式注入测试身份或合适的检查器。不得检测测试环境变量后跳过检查，真实代理集成用例必须经过生产装配路径。

## 4. CLI 模式与优先级

### 4.1 解析规则

先按现有规则找到第一个独立 `--`。其前只解析代理选项；其后由显式模式处理。无分隔符时也只接受现有合法代理选项，不引入 gopls 参数猜测。

| 输入 | 结果 |
|---|---|
| 空 argv | DefaultGopls，默认后端 argv 为 `["mcp"]` |
| `--watch-root ../shared --debounce 500ms` | DefaultGopls，保留指定代理选项 |
| `-- /opt/bin/gopls mcp` | Explicit，后端 argv 为 `["mcp"]` |
| `-- program "" --help --version --` | Explicit，分隔符之后逐项原样传入 |
| `--` 或 `-- ""` | 运行模式下报后端缺失，不退回 DefaultGopls |
| `mcp`、`version`、`program` 等裸位置参数 | 报错并提示显式形式；不当作 gopls CLI 自动转发 |
| 未知代理选项、非法数值 | 保持参数校验失败，不静默启用默认后端 |
| `--help` / `-h` / `--version` | 按现有本地诊断解析规则退出，不进入后端选择 |

显式模式仍具有原始 argv 的边界。默认模式只负责提供一次 `mcp`，不会根据收到的 JSON、协议修订或工具名称调整后端命令。旧 `--gopls`、`--workspace`、`--replace-dir` 的移除状态保持；本轮不重新引入第二套旧 CLI。

启动装配先完成参数校验和本地诊断分支，再捕获 cwd/env 与所需自身身份并解析启动规格；默认定位失败应在创建 tracker 前返回。只有这些本地准备成功后才创建监测器并进入原有进程/协议运行流程。每次 spawn 的最终身份检查仍独立执行，不能用首次解析检查替代。

### 4.2 两种原生配置的迁移

题述没有 args 的配置，实施后只改 command。验收保存修改前后的 TOML，并证明唯一配置差异为 command；该 MCP 条目只显式设置 command 和 default_tools_approval_mode。这不等于证明原始空 argv 的原生 gopls 曾正确提供 MCP。

正确的原生 MCP 对照单独使用 `command=gopls`、`args=["mcp"]`。若用户已有这类配置，迁移默认模式需删除 args，或采用现有显式代理形式；只改 command 且保留 `["mcp"]` 会触发裸位置参数错误。本轮明确只为无 args 配置提供一行替换体验。

## 5. 默认后端定位

### 5.1 自身目录

使用 `os.Executable()` 取得自身路径，并在初始化时解析为绝对物理定位路径；保存其文件身份。不要从 argv[0] 或业务 cwd 推断安装位置。无法建立自身身份时，输出诊断并失败。

默认 sibling 为该物理目录下名为 `gopls` 的文件。若通过另一目录的符号链接启动 proxy，符号链接所在目录的 gopls 不优先；硬链接没有可通用恢复的“原始安装位置”，以当前自身路径的物理目录为准。这两种情况均需测试和文档示例。

### 5.2 搜索与失败规则

1. 对 sibling 调用 Lstat。只有候选确实不存在时才进入 PATH。
2. 候选存在时，验证链接目标、常规文件、Unix 可执行位和自身身份。broken symlink、目录、无执行位、访问错误或自身目标均失败，不换用 PATH 中的另一个版本。
3. PATH 使用代理启动时捕获的值，保持目录顺序。隐式默认模式只检查绝对目录；跳过空组件、`.` 和相对目录，不把这些组件按业务 cwd 展开。PATH 显式包含 cwd 的绝对路径时，仍按普通绝对目录处理。
4. 对每个绝对 PATH 候选重复 Lstat 规则：真正不存在则继续；第一个存在的候选即为绑定候选，其验证失败即终止搜索。访问错误也不能解释为不存在。
5. 没有可用候选时返回明确错误，说明 sibling 和 PATH 搜索均未找到；不遍历文件系统、不运行 shell、不执行 go env、不下载 gopls、不探测 help 猜命令。

Lstat 与后续 Stat 的作用应分开：坏链接本身存在，Stat 的 ENOENT 不得变成“候选不存在”而触发回退。可执行位预检不保证内核一定能执行；格式错误、挂载选项或实际启动失败继续由现有 backend 错误处理负责。

这套较严格的默认选择策略只作用于隐式发现。显式模式对用户指定的路径、相对路径和 PATH 程序名保留当前解析规则；它新增的自身检查不改变 argv 或自动寻找替代程序。

### 5.3 一次选择与后续升级

DefaultGopls 在一次代理运行期间只解析一次候选，保存绝对 locator。定位路径可以保留 gopls 的符号链接；用于验证身份的已解析目标不能替换 locator。

后端删除、损坏或改指自身时，不重新搜索 sibling/PATH。该次启动失败，交给已有有界恢复流程；用户修复原定位路径后，后续合法尝试可以成功。代理重启后才重新执行默认发现。

普通后端升级可以在原 locator 上原子替换文件或切换符号链接；下次原有刷新/重启读取新映像。此次不新增二进制专用 watcher，也不承诺只替换安装目录中的 gopls 就立即刷新。

## 6. 自身启动检查与进程生命周期

### 6.1 身份定义

启动阶段保存 SelfIdentity 的初始文件身份和自身定位路径。选路时以及每次 spawn 时，读取候选当前文件身份，并至少比较：

- 候选规范化路径与自身定位路径；
- 解析符号链接后的身份；
- `os.SameFile` 对启动时自身文件身份的比较，覆盖硬链接；
- 当前自身定位路径的文件身份，覆盖运行期间自身路径被原子升级的情形。

保存初始身份的原因是：只比较当前代理路径的新 inode，可能漏掉仍指向原运行文件的硬链接。身份信息仅用于检查；不重写传给 exec 的后端 locator。

无法取得检查所需的自身或候选身份时，按明确启动失败处理。不得因为解析失败就把检查变成放行。

### 6.2 每次启动的装配

复用现有 Factory 的互斥、boot ID 分配、context 检查、旧进程 cleaned 条件和退出路径。检查在旧后端已确认清理后、新后端执行之前完成，且每次尝试都重新读取候选；尽量靠近实际 cmd.Start。检查失败也消耗该尝试的 boot ID，不发布新 generation。

新增检查失败应走原有启动失败与有界恢复流程，错误写入 stderr；运行中必要的客户端错误继续由现有 proxy 层产生。launcher 不新增 JSON-RPC 错误格式，不自动重放任何请求。guard 后若创建了管道或其他资源，失败路径必须关闭资源。

backend 包的独立 helper 可能用测试可执行文件作为子进程，不能在 backend 中无条件以全局 os.Executable 禁止所有此类测试。注入身份的组件测试和真实生产装配的集成测试分别覆盖，不能设置一个生产可用的“跳过自身检查”环境变量。

### 6.3 明确保证范围

路径查询、身份比较、文件摘要和 exec 是分开的系统调用。本方案检查可观察到的误配置，不声称消除检查到执行之间的文件替换竞态，也不把摘要当作运行映像的安全认证。

任意包装脚本可以间接再次调用 proxy，文件身份比较无法通用识别全部脚本循环。此类自定义包装行为需要显式配置及独立验证；不通过调用 help、读取脚本业务或向子进程注入额外环境标记来猜测。本轮也不承诺覆盖刻意脱离既有进程组管理的后代。

## 7. cwd、环境与输入范围

默认入口只替用户补全后端定位和 `["mcp"]`。子进程继续使用调用方 cwd 与捕获的环境，不添加、删除或改写环境变量，不改变 GOWORK、GOPATH、Go 工具链选择或代理选项默认值。

安装目录不作为项目目录。`--watch-root` 仍只增加输入范围，不 chdir，也不伪造 MCP roots。帮助和版本可以在没有 Go、gopls 或合法项目目录时运行；真正进入代理运行时则沿用 tracker 对有效目录、home 和文件系统根的检查。

| 场景 | 此次约定 |
|---|---|
| Codex 的实际 cwd 是项目目录 | 默认扫描该目录，后端也使用该目录 |
| cwd 是 home、文件系统根或不存在 | 保留现有明确失败；不猜测项目、不扩大扫描到整个 home |
| cwd 外的 replace / go.work use / 资源文件 | 默认不自动覆盖；使用既有 watch-root 或显式选项，并单独验证覆盖 |
| Go discovery | 默认 false；仅用户显式启用时执行既有可选发现 |
| MCP roots 请求与结果 | 继续通用中继，不因默认 launcher 替客户端提供 roots |
| 环境与 PATH 在父进程后续变化 | 一次运行仍用启动时快照；重启代理后重新捕获 |

外部输入场景是支持现有配置能力的范围，不属于“完全不增加配置”的保证。默认发现程序不等于默认发现全部业务输入。

## 8. Codex 预算与验收承诺

继续使用当前 proxy 默认配置，不新增隐式“Codex 模式”或仅针对默认 launcher 的另一组超时。

| 预算 | 文档所列 Codex 默认 | proxy 当前默认 |
|---|---:|---:|
| 启动 | 10s | 45s |
| 工具调用 / 有限请求 | 60s | 110s |
| 一轮刷新恢复 | 客户端仍受其请求预算约束 | 60s，另有最多 6s 最后清理 |

这些数值是上限，不代表每次实际耗时；proxy 不能延长客户端的期限。真实验收记录实际客户端版本和生效预算，分别测量首次扫描、后端创建、原生初始化、工具目录可用、首次调用和刷新后的调用。

不提前伪造 initialize 成功，不跳过完整内容校验，不让旧代次结果穿过校验，也不通过增加目标配置中的超时来把失败改记为“只改 command 通过”。需要额外 cwd、watch-root 或超时的扩展配置可以另行成功，但证据必须标为扩展配置。

冷启动与预热结果分开记录。测试项目、文件数量/字节、缓存状态、并发和机器信息进入证据；未经测量的大项目不在保证内。若默认预算无法满足固定验收基线，先定位实际耗时，在既有正确性约束内优化，或明确未完成该验收，不能只降低 proxy 数值就声称完成。

## 9. 诊断与测试可观测性

启动 stderr 日志记录模式、selectionSource、后端绝对 locator、项目 cwd 和已有 buildinfo；默认 argv 仅为已知 mcp。不要输出完整环境、凭据、业务消息或任意显式 argv，避免把可观测性扩大为参数内容日志。

本地失败诊断至少区分：默认后端未找到、候选不可访问/不可执行、损坏链接、自身引用、无法确定自身身份、显式后端缺失、非法代理参数。稳定的测试 reason 可以保留在内部错误类型；不把这些 reason 作为新增 MCP 工具或新的协议错误标准。

stderr、测试轨迹、缓存与产出均放在监测根之外，避免记录行为引起额外刷新。生产代码不增加仅供测试指定默认 program/argv 的环境变量。零参数集成通过实际文件布局放置名为 gopls 的受控后端，而不是绕过默认解析器。

## 10. 验收分层与发布状态

| 层级 | 证明内容 | 不能替代的结果 |
|---|---|---|
| 参数 / resolver / guard 组件 | 模式、候选顺序、身份、错误和装配规则 | 实际进程及真实客户端 |
| 无自刷新后端零参数集成 | 生产入口的完整启动、原始转发与 proxy 刷新因果 | 真实 gopls / Codex |
| 真实 gopls 零参数集成 | 正确原生 MCP 对照、工具调用、刷新与同一前端会话 | 真实 Codex 配置加载与默认预算 |
| 真实 Codex 目标配置 | 该 MCP 条目只有目标的两个显式设置，合并配置未额外注入启动参数；客户端能发现并调用工具，刷新后使用新内容 | 未测试平台、版本、规模及恢复场景 |
| 打包 / CI | 最终提交对应的二进制与既有回归 | 没有实际运行的平台或客户端 |

真实 Codex 测试需使用独立配置环境，并检查用户级、项目级、profile、插件和受管配置等会影响有效 MCP 启动设置的来源。使用受支持的隔离方法，保留最小必要登录配置；不得修改用户日常 Codex 配置或把凭据收集进证据。日志只保留与该 MCP 条目有关的有效设置及来源。

对照测试允许使用正确的原生 `args=["mcp"]` 配置；目标 MCP 条目只显式设置 command 和 default_tools_approval_mode。Codex 的其他正常配置和默认值可以存在，但不得通过其他配置层增加该条目的 args、env、env_vars、cwd 或超时覆盖。额外夹具进程或 raw harness 的启动条件不能偷偷增加到目标配置。实际生效 argv、cwd、二进制身份、请求和响应结果都需要证据。

自动化 runner 可以显式设置待启动 Codex 的工作目录，因为它是待验证的用户启动场景；不能在目标 MCP 配置中补 cwd，也不能只给 proxy 特殊工作目录。无法固定 Codex 版本、缺少可用客户端/认证或平台不可用时，结果为 pending / blocked，并记录缺失项；不得由 raw harness 推定 pass。

开发阶段可合入已验证的默认入口代码，但若真实 Codex 证据尚缺，必须保留该产品验收状态。只有对应版本、平台和规模的真实验收通过后，才可把“Codex 只改 command”标为已验证能力。

本次实现不创建 tag、Release 或修改发布权限。代码按既有 CI 与打包流程验证，公开 Release 仍需独立发布；新能力不能回填到旧 tag。文档继续区分已实施代码和已验收客户端。

## 11. 取舍与后续范围

本轮选择编译在启动层的 gopls preset，因此需要知道 `gopls mcp` 这一稳定启动约定；工具/schema/命令业务变化不进入代理核心。若未来 gopls 改变 MCP 启动入口，可调整 preset，或继续使用显式通用模式。

如果要求启动层也完全不知道 gopls 的程序名与参数，则必须从外部配置或包装器取得这些信息，不能由没有输入的通用程序推导。本轮不增加 sidecar 配置、环境变量覆盖体系、自动下载、后台守护服务、全 gopls CLI 透传、自动项目选择或新的协议修订支持。

实施中需要调整上述契约时，先在本目录记录具体原因和受影响验收，再修改代码。不得以“只改 command”为由放宽输入正确性、依赖 gopls 自刷新或隐藏客户端超时失败。

## 参考来源

[S1]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/cmd/gopls-mcp-proxy/main.go
[S2]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/cmd/gopls-mcp-proxy/main_test.go
[S3]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/internal/backend/backend.go
[S4]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/internal/backend/backend_test.go
[S5]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/internal/workspace/tracker.go
[S6]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/README.md
[S7]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/internal/proxy/supervisor.go
[S8]: https://github.com/fredgnr/gopls-mcp-proxy/blob/dc9821f76228985333aa590bb1705149d1ba2061/integration/real_gopls_test.go
[S9]: https://go.dev/gopls/features/mcp
[S10]: https://github.com/golang/tools/blob/1f9922d1dcc5836d5c5bcb552c8fed230f885372/gopls/internal/cmd/normalize.go
[S11]: https://learn.chatgpt.com/docs/extend/mcp?surface=cli
[S12]: https://learn.chatgpt.com/docs/config-file/config-reference
[S13]: https://github.com/fredgnr/gopls-mcp-proxy/releases/tag/v0.1.0
