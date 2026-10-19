# 默认 gopls 启动验收计划

本文件是[默认启动设计](design.md)的增量验收，任务安排见[实施计划](implementation-plan.md)。本次只提交方案，以下 **36 个新用例全部为 pending**，没有执行记录，也不报告默认启动已经可用。

审查基线为 [dc9821f76228985333aa590bb1705149d1ba2061](https://github.com/fredgnr/gopls-mcp-proxy/tree/dc9821f76228985333aa590bb1705149d1ba2061)，tree 为 314229ab52794775e5389febf29d15ef910455a8。实际验收必须另记实现 HEAD、tree、构建参数与二进制 SHA-256。[原有协议和刷新验收](../proxy-refresh-protocol/validation.md)及[实际集成断言范围](../../../integration/README.md)继续回归，本文件不把原有用例重新编号，也不把旧通过状态移植到默认入口。

## 1. 判定规则

验收按 CLI/定位组件、实际零参数进程链、真实 gopls、真实 Codex 四层分别给出结论。通过较早一层不能推定后续层通过；实际 Codex 不可用时，其用例保持 pending，并记录 blocker，不以 raw harness 或直接 gopls 测试替代。

“零参数”要求操作系统实际启动 proxy 时，传给 proxy 的参数列表为空（不含 argv[0]），即 `exec.Command(proxyPath)`，没有隐藏的 `-- program argv`；真正的后端参数应为 [mcp]。目标 Codex MCP 条目只显式声明 command 与 default_tools_approval_mode 两个设置；不添加 args、env、env_vars、cwd 或超时覆盖。runner 可以按记录的用户启动场景设置客户端工作目录，但不能只给 proxy 特殊 cwd。

原生互通控制使用 `gopls mcp`。裸上游 gopls 默认是 LSP，因此用户原片段本身是否在其机器上工作仍未知。已经带 args=[mcp] 的原生配置只改 command 后会形成裸位置参数，本轮预期报错；这不属于本次无 args 默认模式的承诺。事实来源和支持条件见[设计基线](design.md#2-事实基线与来源)。

每个用例记录完整 assertion_scope。表中“最低完成阶段”指完整用例可被判定通过的最早阶段；提前完成的组件断言只能单独记录，不能把整个用例标成 pass。

## 2. 测试设施与证据

### 2.1 受控安装布局

构建真正的 proxy 可执行文件；不要用 go run 的临时 executable 路径验证同目录发现。另建能接受唯一参数 mcp 的独立 gopls 夹具，并按用例放在以下位置：

- proxy 物理目录中的 sibling gopls；
- 一个或多个 PATH 绝对目录；
- proxy 启动 symlink 所在的另一目录，以及硬链接启动路径所在目录；
- 含空格、Unicode 的目录；指向合法目标、损坏目标或 proxy 的符号链接与硬链接。

默认发现只读取正常继承的 PATH。允许在测试准备时布置受控 PATH 目录；禁止通过额外环境变量指定默认 backend 路径、argv、resolver 返回值或 Guard 绕过开关。输入清单和证据目录可以作为 fixture 数据传递，但不能影响生产启动决策。零参数测试不能复用原 raw suite 注入显式 test-executable 与缩短时间参数的命令。

权限失败必须真实触发。无执行位、目录、FIFO、Lstat/Stat 权限错误分别断言；需要权限隔离的测试在普通用户下运行，或在组件层注入明确的系统调用失败。以 root 身份无法复现 EACCES 时，不可把未触发分支记成通过。

### 2.2 不自刷新的独立后端

fixture 不导入生产 wire/protocol，也不使用与 proxy 共享的 MCP SDK 编解码。每个新进程启动时读取一次测试输入，随后所有查询只返回内存快照。记录随机 boot ID、真实 PID、读取次数、原始帧和业务执行次数。

先直接运行 fixture：写入 A→B 后，同一个 PID 必须继续返回 A，读取次数不增加；重启后的进程才返回 B。再启动无参数 proxy 验证 B 的可见性来自新后端 boot，而非后端自己读取磁盘。零参数 fixture 的内容范围与现有 raw suite 一致，至少覆盖普通 .go 内容及 go.mod、go.sum、go.work、go.work.sum 的连续变化。

真实 gopls 的行为作为互通证据，不作为独立刷新因果证据。真实调用结果发生变化，若没有新 boot、同一 proxy PID 及不自刷新 fixture 的对应证据，不能推出刷新由 proxy 正确完成。

### 2.3 记录格式与目录

全部 transcript、stderr、配置快照、执行记录、构建输出和计时文件放在 watchroot 外。默认监测常规文件内容；在被测项目内写日志会产生额外变更，使“仅改 command”和刷新计时结果失真。

每次运行至少保存下列信息，敏感环境值和凭据不进入记录：

| 信息组 | 必需内容 |
|---|---|
| 用例定位 | case_id、task_id、phase、revision、assertion_scope、status、run_id、run_attempt、开始/结束时间 |
| 版本身份 | 实现 HEAD、tree、工作树状态；proxy、fixture、gopls 的路径与二进制 SHA-256；构建参数、buildinfo、Go/gopls/Codex 版本 |
| 平台与场景 | OS/架构、文件系统、实际用户权限、机器资源、项目路径与规模、文件数/字节、缓存状态、并发 |
| 启动事实 | 实际可执行路径和 argv、selectionSource、固定 backend locator、proxy PID、backend PID/boot、关键生效配置及来源 |
| 内容与执行 | 输入版本与指纹、每 boot 读取次数、相关原始双向帧、业务执行次数、代次发布及失败退出记录 |
| 时间与结果 | 启动/扫描/初始化/工具发现/工具响应/刷新时间点、实际生效预算、失败原因、cleanup 结果、未覆盖范围 |

真实环境的 argv/env 只保存完成核对所必需的脱敏内容；完整值一致性的自动化断言使用安全的合成数据。不得输出完整环境或把任意显式 argv 加到生产日志。用来证明两份 TOML 只差 command 的文件也须去除与该 MCP 条目无关的敏感全局信息。

## 3. 增量用例矩阵

### 3.1 参数和本地诊断

| 用例 | 最低完成阶段 | 场景与硬断言 | 状态 |
|---|---|---|---|
| VAL-LAUNCH-001 | L1 | 空 argv 得到 DefaultGopls 意图，后端默认 argv 恰为 [mcp]；不从请求内容猜启动方式。parseOptions 的旧 nil 错误断言替换为该断言。 | pending |
| VAL-LAUNCH-002 | L1 | 只有合法 proxy 选项且无分隔符时进入默认模式，所有指定参数及其余默认值保持；覆盖重复 watch-root、include/exclude、时长和布尔选项。旧“合法 watch-root 但无后端必错”断言替换。 | pending |
| VAL-LAUNCH-003 | L1 | 显式分隔符后的 program、空 argv、Unicode/空格、引号、字面 shell 字符、未知参数、help/version 和第二个分隔符逐项原样且克隆；不额外插入 mcp。显式相对路径和程序名不在参数解析时重写，实际解析回归见 015/030。 | pending |
| VAL-LAUNCH-004 | L1 | 显式 `--`、`-- ""` 仍报缺后端，不转入默认发现。错误不依赖机器上是否装有 sibling gopls。 | pending |
| VAL-LAUNCH-005 | L1 | 裸 mcp/version/program、其他位置参数、未知选项、非法时长、旧 gopls/workspace/replace-dir 选项均失败，给出有效迁移指引；args=[mcp] 只改 command 的配置明确不通过。 | pending |
| VAL-LAUNCH-006 | L2 | help/-h/version 在没有 Go、没有后端、无有效项目 cwd 的受控环境下成功退出；用注入计数或外部探针证明不定位后端、不捕获运行身份、不创建 tracker、不运行 Go、不 spawn。输出限于 stderr。 | pending |

### 3.2 默认定位和身份

| 用例 | 最低完成阶段 | 场景与硬断言 | 状态 |
|---|---|---|---|
| VAL-LAUNCH-007 | L1 | 可用 sibling 存在且 PATH/cwd 有不同 gopls，绑定物理 proxy 目录中的 sibling；argv[0] 文本不能改选择。记录候选路径与选择来源。 | pending |
| VAL-LAUNCH-008 | L3 | 从另一目录的 proxy symlink 启动实际构建二进制，选择物理目标目录中的 gopls，忽略 symlink 旁的竞争版本。另从 hardlink 启动，按可获得的当前物理路径目录选择，不虚构“原始安装目录”。 | pending |
| VAL-LAUNCH-009 | L1 | sibling 的 Lstat 真正 ENOENT 时，按 PATH 绝对目录顺序继续；不存在候选可跳过，首个存在且可用的候选被绑定。空格/Unicode 路径作为普通路径，无 shell 解释。 | pending |
| VAL-LAUNCH-010 | L1 | sibling 是坏 symlink 时，Lstat 的“存在”与 Stat 的 ENOENT 区分；明确报错，绝不回退到可用 PATH 候选。 | pending |
| VAL-LAUNCH-011 | L1 | sibling 为目录/FIFO/非可执行文件，或其 Lstat/Stat/访问发生权限错误时失败；不得视作缺失或回退。执行格式等内核启动错误另由既有 backend 路径处理。 | pending |
| VAL-LAUNCH-012 | L1 | 默认 PATH 中空组件、点目录、其他相对目录均跳过，不能将它们按 cwd 展开；仅这些位置存在 gopls 时应未找到。反例：PATH 明确包含与 cwd 相同的绝对目录时，按普通绝对项参与选择。 | pending |
| VAL-LAUNCH-013 | L1 | PATH 第一个 Lstat 存在的 gopls 为坏链接/无权限/非可执行/目录/FIFO 时即失败，不跳到后续可用版本。访问错误同样终止；self 的完整生产拒绝另见 016。 | pending |
| VAL-LAUNCH-014 | L2 | sibling 与所有合规 PATH 候选均不存在，或无法确定 proxy 物理路径/身份时有明确失败；不创建 tracker 扫描项目、不扫描文件系统找程序、不执行 shell/go env、不下载后端、不尝试 help 猜命令。stdout 无诊断文字。 | pending |
| VAL-LAUNCH-015 | L1 | 单次 resolver 调用选择合法 gopls symlink 后返回绝对 locator，保留该 symlink 路径，身份检查取得的目标路径不能覆盖 locator。显式 resolveExecutable 不采用本次严格 PATH 规则；全会话只选择一次由 022 验证。 | pending |
| VAL-LAUNCH-016 | L2 | 按可构造的路径组合覆盖直接 self、symlink self 和 hardlink self；默认 sibling、默认 PATH 首个候选、显式后端都经过生产检查，在实际 spawn 前拒绝已观察到的自身身份。不得继续搜索其他候选或生成递归 proxy。 | pending |
| VAL-LAUNCH-017 | L2 | proxy 启动后替换其自身 locator，Guard 同时识别旧启动 inode 的 hardlink 和当前自身 locator 的新身份。任一必需身份无法读取即失败；验证失败消耗启动 attempt、不发布新 generation。 | pending |
| VAL-LAUNCH-018 | L2 | 组件和 backend helper 测试注入独立 selfIdentity/Guard，合法使用测试可执行文件仍可启动；真实 CLI 两种模式都装配生产 Guard，无测试环境变量绕过分支。 | pending |

### 3.3 固定定位、升级与零参数刷新

| 用例 | 最低完成阶段 | 场景与硬断言 | 状态 |
|---|---|---|---|
| VAL-LAUNCH-019 | L3 | 首次选择的 locator 在后续 spawn 前原子替换为另一合法 fixture 二进制，或 symlink 换到合法目标；两份 fixture 的初始化信息、目录及状态满足既有会话恢复条件，同一前端连接中下一次既有刷新使用新映像与新 boot。仅改二进制不要求立即触发刷新，测试另改已监测输入。协议约定不兼容时仍按既有规则重连，不要求本用例强行保持会话。 | pending |
| VAL-LAUNCH-020 | L3 | 正常启动后把同一 locator 改为 proxy 本身、proxy symlink 或 hardlink，再触发刷新；Guard 在执行前拒绝，不递归启动、不切换到其他 PATH 版本、不返回旧代次成功、不重放已派发业务。 | pending |
| VAL-LAUNCH-021 | L3 | 后续 locator 被删除、改为坏链接/无执行位/非法类型，或校验所需路径不可访问时，沿用既有有界恢复与清理；不重新发现后端。恢复窗口内修复原 locator 后，后续合法尝试可按既有恢复契约成功。 | pending |
| VAL-LAUNCH-022 | L3 | 首次经 PATH 绑定后再加入优先 sibling/更前 PATH 候选，或修改调用方持有的原始 cfg/env/argv 切片，后续 spawn 仍使用固定绝对 locator 和快照。重启整个 proxy 后才重新发现。 | pending |
| VAL-LAUNCH-023 | L3 | 真构建 proxy 与名为 gopls 的独立 fixture 放在 sibling 布局，实际无参数启动 proxy；后端恰收 [mcp]，初始化/工具发现/工具调用走实际 stdio。无显式后端、无默认后端环境覆盖。 | pending |
| VAL-LAUNCH-024 | L3 | sibling 确实不存在，在绝对 PATH 布局重复实际零参数全链路；证明运行的是预期候选，且没有替代成旧显式 harness。 | pending |
| VAL-LAUNCH-025 | L3 | 只有合法 proxy 选项的入口实际启动默认后端；watch-root 增加监测范围而不 chdir、不改变 mcp argv；include/exclude 和 GoDiscovery 显式选择维持原语义。此例不作为无参数/三行 Codex 通过证据。 | pending |
| VAL-LAUNCH-026 | L0 | 独立 fixture 自证：直接同 PID 运行，磁盘 A→B 后仍返回 A，输入读取次数固定为启动一次；新 PID 才返回 B。新默认入口 fixture 和原 helper 的复用不能破坏该性质。 | pending |
| VAL-LAUNCH-027 | L3 | 无参数 proxy 通过不自刷新 fixture 检验 .go 与四类依赖文件至少连续两次变化，含等长内容及恢复 mtime；同一 proxy PID/前端连接、新 backend boot/PID、新快照与每 boot 单次读取相符。已派发阻塞请求遇刷新不自动 replay，以独立执行次数判定。 | pending |
| VAL-LAUNCH-028 | L3 | 默认入口继承安全合成 env 和 cwd，不因安装目录推断项目、不改 GOWORK/工具链选择；GoDiscovery 默认为 false，不调用 Go 发现。默认扫描范围与 home/root 拒绝保持，外部 replace/use 输入不虚构覆盖；MCP roots 继续来自客户端。 | pending |
| VAL-LAUNCH-029 | L3 | 新启动/错误日志只到 stderr，stdout 保持原始协议帧；使用未知字段、schema、字符串/数字 ID 与后端错误的现有断言检查装配未改正文。错误日志不包含完整 env 或任意显式 argv。 | pending |
| VAL-LAUNCH-030 | L2 | 原有 CLI、backend、workspace、raw 协议测试全部按原断言回归，除 001/002 指定旧错误断言外不削弱。覆盖显式路径、symlink 升级、boot attempt、取消、FIFO/摘要、清理、反向请求、版本恢复、长期状态和已派发请求不重放。 | pending |

VAL-LAUNCH-027 的独立因果是本次默认入口增量的核心。旧协议套件对漏 watcher 事件、轮询、ID 复用、取消和长期状态等仍为必需回归；本次不重复编写全部历史协议用例，也不能因默认入口测试通过而停跑这些套件。

### 3.4 真实 gopls、真实 Codex 和交付证据

| 用例 | 最低完成阶段 | 场景与硬断言 | 状态 |
|---|---|---|---|
| VAL-LAUNCH-031 | L3 | 在记录版本的真实 gopls v0.24.0 布局中无参数启动 proxy；与合法原生 gopls mcp 控制比较稳定 tools/list 并调用已支持工具。gopls 路径只用于安装准备，运行时不作为额外 backend env/argv 注入。 | pending |
| VAL-LAUNCH-032 | L3 | 默认无参数入口运行 protobuf 1.34.2→1.35.2→1.36.6 和 .go 内容变更，保持同一前端连接/proxy PID，工具结果含预期版本内容并记录换代。结论限真实互通；独立刷新正确性仍引用 026/027。 | pending |
| VAL-LAUNCH-033 | L4 | 真实 Codex 使用独立合法原生控制配置 command=gopls、args=[mcp]，在相同项目 cwd/环境发现并调用工具。登记客户端与后端版本、实际生效设置；不得用裸 gopls 作为已工作的原生控制。 | pending |
| VAL-LAUNCH-034 | L4 | 真实 Codex 用题述无 args 目标配置，只把 command 改成 proxy；目标条目只显式声明两键，合并后无 args/env/env_vars/cwd/timeout 覆盖。验证发现、调用、受监测内容变化后的调用及同一 proxy 会话；留存配置差异和实际 argv。缺客户端或真实证据时保持 pending。 | pending |
| VAL-LAUNCH-035 | L4 | 在 034 原封不动的目标配置下分别测量冷启动/首次扫描/后端启动/initialize/tools-list/普通调用/刷新后调用；记录实际 Codex 默认启动 10s、工具 60s 与 proxy 内部预算。冷暖分开，超时不加配置伪装通过，不减内容校验换速度。 | pending |
| VAL-LAUNCH-036 | L4 | Linux/macOS、Go 1.25.x/1.27.2 的既有矩阵实际选中增量测试，真实工具行使用 gopls v0.24.0；保留 race、只读 CI、persist-credentials:false 和证据上传。以 JSON 确认运行而非静默 skip，并把 HEAD/tree/hash/平台/attempt 与所有结论对应；真实 Codex 状态独立保留。 | pending |

## 4. 真实 Codex 的具体执行契约

合法原生控制配置单独保存，例如：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls"
args = ["mcp"]
```

目标配置单独保存，例如：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls-mcp-proxy"
```

以上路径是布局示例，不能代替实际机器的版本和文件验证。题述原配置与目标配置的差异只允许 command 的值变化；原生控制另有 args=[mcp]，不要将控制配置到目标配置的差异误写为“仅 command 改动”。

使用实际客户端支持的隔离方式，不修改日常配置。检查用户级、项目级、profile、插件及受管设置对该 MCP 条目的影响；目标条目只能显式声明 command 和 default_tools_approval_mode，合并后的 args/env/env_vars/cwd/timeout 不得被其他来源补入。模型、登录等必要全局配置可以保留，凭据不进入产物。客户端默认值仍会存在，并记录实际生效值。

测试 runner 为客户端选择固定项目 cwd，控制和目标一致。项目必须符合现有监测根约束，全部验收日志放在外部目录。确认实际运行的 proxy/gopls 文件、argv 和版本，避免旧 Release 或同名 wrapper 使测试入口与被审查实现不一致。执行不涉及给其他人发送消息、发布代码或修改用户日常项目。

完整运行应依次完成配置加载、MCP 建连、工具发现、至少一个工具调用、可控源码修改和后续工具调用；跨刷新保持同一 proxy 进程与前端会话。客户端若自行重连，记录真实现象并按断言失败或受限处理，不能把新会话冒充无断连刷新。

已有 args=[mcp] 的原生配置另测迁移错误：仅改 command 后报裸位置参数；删除 args 使用默认模式，或选择显式 `-- program argv`，属于另一种配置变更。只要目标条目增加了 args、env、env_vars、cwd、watch-root 或超时设置，即归入扩展配置实验，不能作为 VAL-LAUNCH-034 的通过证据。

## 5. 默认预算与性能记录

文档核对时 Codex 默认启动预算为 10s、工具预算为 60s；基线 proxy 默认 startup 为 45s、request 为 110s、refresh 为 60s，final cleanup 为 6s，GoDiscovery 为 false。来源见[设计的预算说明](design.md#8-codex-预算与验收承诺)。两个进程的预算分别生效，proxy 较长的内部等待不能延长客户端期限。

至少记录以下实际时间点，采用同一可关联的单调时钟或说明跨进程对时方法：

| 时间点 | 用途 |
|---|---|
| 客户端发起启动 / proxy 创建 | 定位完整的客户端启动耗时起点 |
| 默认定位完成 / 首次扫描开始与完成 | 分离 launcher 定位、项目内容扫描与后端初始化成本 |
| backend 创建 / 原生 initialize 完成 | 定位进程创建及 gopls 初始化成本 |
| tools/list 响应 / 首次工具响应 | 判断客户端可见的发现与调用是否完成 |
| 输入修改 / 变化确认 / 新代次可用 / 刷新后响应 | 分离检测、清理、重启、恢复和实际工具耗时 |
| 客户端 timeout 或退出 / 子进程全部清理 | 记录失败的真实边界及是否遗留进程 |

若实际协议分支没有 initialize，把该阶段标为不适用，不能向原始协议插入测量消息。时间点可由受控测试观测和 stderr 记录取得，不修改原生协议字段或向 stdout 写计时信息。

至少分别运行并标记冷启动和预热场景；登记项目规模、依赖/Go 缓存状态、下载行为、文件数量与字节、并发、平台工具版本。报告原始样本及次数，样本不足时不声称稳定分位数。默认预算下失败的场景保留原始结果；允许另做扩展超时诊断，但不把扩展配置结果替换回默认目标。

如果大型项目的首次扫描、依赖载入或恢复超出客户端预算，先定位具体阶段。任何优化继续满足既有内容正确性和不重放约束。不能通过跳扫描、依赖 gopls 自刷新、返回旧快照或提前伪造初始化完成来满足时间要求。

## 6. 需求、任务与最终门禁映射

| 设计要求 | 本次增量证据 |
|---|---|
| DL-R01 | VAL-LAUNCH-001、002、023～025 |
| DL-R02 | VAL-LAUNCH-003～005、015、030 |
| DL-R03 | VAL-LAUNCH-007～015、019～022 |
| DL-R04 | VAL-LAUNCH-016～018、020、021 |
| DL-R05 | VAL-LAUNCH-003、023～025、028、029 |
| DL-R06 | VAL-LAUNCH-006、014、029 |
| DL-R07 | VAL-LAUNCH-017、020、021、026、027、030 |
| DL-R08 | VAL-LAUNCH-023、024、026、027、031～035 |
| DL-R09 | VAL-LAUNCH-003、015、018、019、030、036 |
| DL-R10 | 所有执行记录的版本身份，以及 VAL-LAUNCH-034～036 |

具体任务 ID 到用例的映射在[实施计划](implementation-plan.md#3-任务清单)。L0 的夹具自证不等于默认启动已实现；L1 的 parser/resolver 断言不等于完整进程链通过；L3 的真实 gopls 结果不等于真实 Codex 通过。

自动化平台矩阵保持 Linux Go 1.25.x、Linux Go 1.27.2、macOS Go 1.27.2；后两行安装真实 gopls v0.24.0。基线 CI 只选 `^TestRaw` 和 `^TestRealGopls`，新集成命名应落入选择器，或同步调整选择器并验证实际 JSON 结果。必需 fixture 测试不能因缺 gopls 而 skip；专用真实后端 CI 缺二进制/依赖或下载失败应失败。平台权限、硬链接能力等缺失必须记录，不能以某个平台的组件模拟替代另一个平台完整通过。

新增入口代码可按已经通过的工程验证单独合入，但产品结论必须保留实际状态。没有 VAL-LAUNCH-034、035 的真实 Codex 证据时，写“默认入口代码已验证，Codex 目标验收 pending/blocked”，不能写“只改 command 已完全完成”。将来可声明的范围应列明实际通过的平台、客户端/后端版本、项目条件和默认预算；本次文档 PR 不产生任何运行成功或发布声明。

