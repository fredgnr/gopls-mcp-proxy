# 默认 gopls 启动实施计划

本计划对应[方案入口](README.md)、[设计契约](design.md)和[增量验收](validation.md)。本次提交只定义后续开发任务，不改变运行时代码。以下 18 项任务全部为 **pending**；旧功能的通过记录不能作为本次默认启动已经实现的证据。

审查基线为提交 [dc9821f76228985333aa590bb1705149d1ba2061](https://github.com/fredgnr/gopls-mcp-proxy/tree/dc9821f76228985333aa590bb1705149d1ba2061)，对应 tree 为 314229ab52794775e5389febf29d15ef910455a8。后续实现和验收必须记录实际 HEAD、tree 与被测二进制哈希，不能把本方案基线误记成实现版本。

## 1. 实施原则与阶段边界

默认启动只增加一种明确的入口：无参数，或只有合法 proxy 选项且没有分隔符时，自动定位 gopls，并传入唯一后端参数 mcp。显式形式仍为 `gopls-mcp-proxy [proxy options] -- program [argv...]`。出现分隔符但没有后端不能转入默认模式；裸 mcp 和其他位置参数仍报错。

默认发现与显式后端解析分别实现。默认发现只查看 proxy 物理可执行文件所在目录和 PATH 中的绝对目录，并按设计契约处理第一个存在的候选。显式后端继续使用现有路径与 argv 规则。两种入口都由生产 CLI 装配每次启动的自身检查，不在通用 backend 库内无条件把测试可执行文件当作禁止启动的后端。

阶段按依赖推进：

| 阶段 | 本阶段交付 | 本阶段可证明的范围 | 后续依赖 |
|---|---|---|---|
| L0 | 契约、独立夹具、证据目录及基线清单 | 夹具的启动一次读取和证据完整性；其他用例仅完成准备 | L1 |
| L1 | CLI 解析、默认定位、身份检查组件 | 组件输入输出和错误边界 | L2 |
| L2 | 生产 CLI 与每次 spawn 的集成 | 两种入口均正确装配；通用后端及旧显式调用回归 | L3 |
| L3 | 真正零参数进程链和真实 gopls 互通 | 实际安装布局、刷新因果、后端替换及原生互通 | L4 |
| L4 | 真实 Codex、默认预算、CI 与用户文档 | 指定平台和项目条件下的 command-only 配置验收 | 最终结论 |

每条执行记录同时填写 phase、case_id、revision、assertion_scope、status。组件测试完成时，只记录其明确断言；尚未运行完整进程链的用例不能整体标记通过。L1 不以尚待 L2 装配的 spawn 行为作为门禁，L2 不以真实 Codex 可用为门禁。完整用例的最低完成阶段见[验收矩阵](validation.md#3-增量用例矩阵)。

## 2. 当前源码与改动落点

下表中的新增路径是实施建议，可在保持职责边界的前提下调整；路径变动后须同步任务与验收映射。

| 文件或目录 | 基线行为与本次工作 |
|---|---|
| [cmd/gopls-mcp-proxy/main.go](../../../cmd/gopls-mcp-proxy/main.go) | parseOptions 目前要求分隔符和 program。增加默认/显式启动意图，合法性校验仍先于启动。help/version 提前返回。run 组装默认定位结果与 Guard，保持 cwd、env、tracker 配置。 |
| [cmd/gopls-mcp-proxy/main_test.go](../../../cmd/gopls-mcp-proxy/main_test.go) | TestProxyHelpVersionAndMissingProgram 当前把 nil 参数、合法 watch-root 选项但没有分隔符列为错误。只替换这两类断言；显式缺后端、非法选项和位置参数继续失败。保留原始 argv 克隆、旧参数迁移提示及无 Go 的 version 检查。 |
| 建议新增 internal/launcher/resolver.go、resolver_test.go | 纯定位组件：物理 proxy 目录、Lstat/Stat 分类、严格默认 PATH、固定绝对 locator。不得修改通用显式解析以复用默认 PATH 限制。 |
| 建议新增 internal/launcher/identity.go、identity_test.go | 捕获 proxy 启动时文件身份，并保存其物理自身 locator；每次 spawn 同时比较启动快照和当前自身 locator 身份。提供普通依赖注入供测试使用。 |
| [internal/backend/backend.go](../../../internal/backend/backend.go) | NewFactory 已固定 locator、克隆 argv/env；Start 已逐次读取可执行文件摘要。本次增加可注入的 spawn Guard，保留显式 resolveExecutable 行为、boot attempt 计数和清理边界。摘要继续是观测信息，不是执行身份的安全证明。 |
| [internal/backend/backend_test.go](../../../internal/backend/backend_test.go)、[process_test.go](../../../internal/backend/process_test.go) | 现有测试用测试可执行文件充当后端，须注入与该 helper 不同的合法 selfIdentity。保留 symlink 升级、失败启动计数、FIFO 拒绝、取消摘要、子进程组清理等测试。 |
| [internal/testbackend/helper.go](../../../internal/testbackend/helper.go)、[integration/raw_proxy_test.go](../../../integration/raw_proxy_test.go) | 复用独立、启动后不自刷新的协议夹具；新增能以文件名 gopls、argv=[mcp] 启动的测试入口及实际安装布局。现有显式测试入口与时间覆盖保留。 |
| [integration/real_gopls_test.go](../../../integration/real_gopls_test.go) | 新增零参数 proxy 入口的 pinned gopls 互通和刷新样例；已有显式后端两次 protobuf 升级测试继续回归。 |
| [internal/proxy/supervisor.go](../../../internal/proxy/supervisor.go)、[internal/workspace/tracker.go](../../../internal/workspace/tracker.go) | 本次不调整协议恢复、输入内容正确性、监测根和预算策略；以回归断言守住这些边界。 |
| [.github/workflows/ci.yml](../../../.github/workflows/ci.yml)、[integration/README.md](../../../integration/README.md)、[README.md](../../../README.md) | 接入增量用例和证据；保留现有平台、版本、安全权限及通用协议用例。文档分别描述用户无 args 模板和已有 args=[mcp] 模板的迁移边界。 |

现有 raw 集成测试通过显式 `-- test-executable ...` 启动 helper，并覆盖 proxy 的若干时间参数。这套用例继续证明旧入口及协议行为，但不直接证明“只改 command”或默认预算。新增零参数套件必须实际构建 proxy 和独立的 gopls 夹具，不能以 go run 的临时可执行文件布局替代安装布局。

## 3. 任务清单

### L0：固定契约、独立夹具与证据

| 任务 | 工作与交付 | 依赖及验收映射 | 状态 |
|---|---|---|---|
| TP-L0.1 | 固定默认/显式启动意图、定位结果、错误分类、selfIdentity 和 Guard 的接口契约。写明默认 PATH 的“首个 Lstat 存在者绑定”规则、双重自身身份比较及未知身份失败规则。 | 无前置依赖；为 VAL-LAUNCH-001～022 准备断言，不标记这些完整用例通过。 | pending |
| TP-L0.2 | 建立独立 gopls 夹具入口。入口只接受预期 mcp 参数，每个 boot 只读一次内容，保留原始帧、执行次数、boot/PID。直接同进程写 A→B 后仍返回 A，新进程才返回 B。 | TP-L0.1；VAL-LAUNCH-026。 | pending |
| TP-L0.3 | 建立 sibling、PATH、启动 symlink、hardlink、损坏候选和升级布局。构建/日志/计时/配置证据放在 watchroot 外，预留 HEAD/tree/hash/run attempt 等记录字段；权限用例确保实际运行身份能复现。 | TP-L0.1；准备 VAL-LAUNCH-007～024、028、035、036 的夹具与记录，不提前算通过。 | pending |
| TP-L0.4 | 独立审阅夹具和契约：默认后端路径、argv、resolver 结果不能借 env 或测试专属生产开关注入；fixture 输入和证据定位通道不能改变代理的默认行为。核对旧断言的最小替换范围。 | TP-L0.2、TP-L0.3；L0 门禁为 VAL-LAUNCH-026 自证和审阅记录。 | pending |

L0 可以为 fixture 提供合成输入清单、证据目录等测试数据。禁止借这些通道设置默认 backend executable、默认 argv 或偷偷增加 proxy 选项。生产代码不能增加根据测试环境变量跳过 Guard、绕过定位或改变运行模式的分支。

### L1：CLI、定位与身份组件并行实现

| 任务 | 工作与交付 | 依赖及验收映射 | 状态 |
|---|---|---|---|
| TP-L1.1 | 修改 main 的参数解析与 usage：无分隔符的合法选项产生默认意图；显式分隔符后的 argv 逐项保留；缺后端、裸 mcp、位置参数、旧参数及非法配置继续失败。help/version 先返回，不解析后端。 | TP-L0.4；VAL-LAUNCH-001～005；VAL-LAUNCH-006 的解析子断言。 | pending |
| TP-L1.2 | 实现默认 resolver：os.Executable 经 EvalSymlinks 定位物理目录；所有候选先 Lstat。sibling 仅真实 ENOENT 才进入 PATH；PATH 只考虑绝对目录，绑定第一个存在的 gopls，候选不可用即失败。返回原符号路径的绝对 locator，不冻结其目标。 | TP-L0.4；VAL-LAUNCH-007、009～013、015；VAL-LAUNCH-008、014 的组件子断言。 | pending |
| TP-L1.3 | 实现身份快照和 Guard 组件：比较候选当前文件身份与 proxy 启动快照、当前自身 locator 身份；覆盖 direct/symlink/hardlink 和无法完成校验。测试注入独立 selfIdentity，不在 backend 层自行猜测。 | TP-L0.4；VAL-LAUNCH-016～018 的组件子断言。 | pending |

TP-L1.1 主要拥有 main 及其测试；TP-L1.2 主要拥有 resolver 文件；TP-L1.3 主要拥有 identity 文件。三项可按 L0 接口约定并行。遇到公共类型改动先协调单一修改者，避免为并行重复实现定位逻辑。L1 组件通过后再进入统一装配，不用尚未存在的 CLI 端到端结果阻塞某一个组件。

### L2：生产启动链与逐次 spawn 集成

| 任务 | 工作与交付 | 依赖及验收映射 | 状态 |
|---|---|---|---|
| TP-L2.1 | 在 run 中组装启动意图。默认模式生成固定 locator 和 argv=[mcp]，显式模式保留原始 program/argv 路径规则。help/version 在捕获自身身份、查后端、运行 Go 和创建 tracker 之前返回；本地参数与定位校验失败也不进入项目扫描。cwd/env 原样进入现有 tracker/backend 配置。 | TP-L1.1～L1.3；完整 VAL-LAUNCH-006、014；VAL-LAUNCH-023～025、028 的组件断言。 | pending |
| TP-L2.2 | 将生产 Guard 注入 Factory，每次启动在执行前检查候选及自身身份；启动和重启失败继续受现有预算、boot attempt 与 cleanup 规则约束。全会话不重搜 sibling/PATH；保留 gopls symlink locator 以允许升级。 | TP-L2.1；完整 VAL-LAUNCH-016～018；VAL-LAUNCH-019～022 的组件断言。 | pending |
| TP-L2.3 | 跑通现有 generic backend、CLI、workspace、raw 协议回归；核对默认 PATH 限制未泄漏到显式入口。检查新错误只写 stderr，不带完整 env，不产生额外协议输出或业务重放。 | TP-L2.2；VAL-LAUNCH-030；VAL-LAUNCH-029 的组件断言。 | pending |

L2 不重写协议解析或 gopls 工具目录，不引入隐藏的 CLI 自动转发规则。裸 `gopls-mcp-proxy mcp` 仍不能替代显式模式；后端 help/version 仍需位于显式分隔符后。Guard 降低直接或文件别名指向 proxy 的循环风险；它不能消除校验与 exec 之间的 TOCTOU，也不保证识别任意 wrapper 的间接循环。

### L3：真正零参数进程链和真实后端互通

| 任务 | 工作与交付 | 依赖及验收映射 | 状态 |
|---|---|---|---|
| TP-L3.1 | 以实际构建文件创建 sibling/PATH 安装布局，直接启动 proxy 且 args 长度为 0。验证物理目录、自身启动 symlink/hardlink、mcp 唯一参数、cwd/env/roots 和纯 stdio。另建只有合法 proxy 选项的入口用例。 | TP-L2.3、TP-L0.2；VAL-LAUNCH-008、023～025、028、029。 | pending |
| TP-L3.2 | 用同一个前端连接与 proxy PID 运行无自刷新夹具的内容变化、后端目标升级和后续指向 self 的故障。核对 boot、真实执行记录和固定 locator，失败时不选择另一个版本、不自动 replay 已派发请求。 | TP-L3.1；VAL-LAUNCH-019～022、027。 | pending |
| TP-L3.3 | 把实际 gopls v0.24.0 安装或复制到受控默认布局，零参数启动 proxy。使用原生 gopls mcp 作独立互通对照，覆盖工具发现/调用、protobuf 两次升级与源码变化。 | TP-L3.1、TP-L3.2；VAL-LAUNCH-031、032。 | pending |

真实 gopls 可以有自己的刷新能力，因此 TP-L3.3 只证明实际工具的兼容性和默认入口互通；独立的刷新因果仍由 TP-L3.2 的不自刷新夹具证明。辅助程序安装或测试准备阶段可以查找 pinned gopls，但实际 proxy 命令必须无参数，且不得通过 env 向生产默认 resolver 传入选定后端路径。

### L4：真实 Codex、默认预算和交付门禁

| 任务 | 工作与交付 | 依赖及验收映射 | 状态 |
|---|---|---|---|
| TP-L4.1 | 在隔离且可审计的有效 Codex 配置下，先验证合法原生控制配置，再验证题述无 args 模板只改 command 的目标配置。目标 MCP 条目只显式声明 command、default_tools_approval_mode 两键，合并后没有额外 args/env/cwd/超时覆盖。记录客户端版本、实际加载的配置、cwd 与真实协议/进程证据。 | TP-L3.3；VAL-LAUNCH-033、034。客户端不可用时保留 pending。 | pending |
| TP-L4.2 | 不增加 timeout 配置，测量冷启动、首次扫描、initialize、tools/list、普通工具及刷新后的请求。分别报告 Codex 默认启动 10s、工具 60s 与 proxy 现有内部预算的实际效果。 | TP-L4.1；VAL-LAUNCH-035。无法在默认预算内完成即不能宣称该测试条件下 command-only 成立。 | pending |
| TP-L4.3 | 把默认启动用例纳入 Linux/macOS 和既有 Go/gopls 版本门禁，核对测试选择器确实选中。保留只读 CI 权限、checkout persist-credentials:false、固定 Actions 版本和原有证据上传；缺必需工具不得静默 skip。 | TP-L3.3；VAL-LAUNCH-036 的自动化和跨平台部分；可与 TP-L4.1、TP-L4.2 并行。 | pending |
| TP-L4.4 | 更新 README、integration/README 及帮助文本；分别说明无 args 默认启动、显式泛用模式、已有 args=[mcp] 配置迁移，以及默认 cwd/监测范围和安装布局。按实际运行结果填写案例状态与限制。 | TP-L4.1～L4.3 结果；VAL-LAUNCH-034～036 的文档和结论映射。未运行项目仍 pending。 | pending |
| TP-L4.5 | 独立复核源码、任务与证据对应关系。确认二进制来自被审查 HEAD/tree，所有必需平台的完整断言通过，真实 Codex 结果没有被 raw harness 替代；形成最终验收结论。 | TP-L4.4；汇总 VAL-LAUNCH-001～036，不新增替代性成功标准。 | pending |

### 子代理协作

主代理负责接口冻结、依赖推进、集成与最终结论；在可用并发槽内把文件边界明确的工作交给子代理。模型和推理设置继承主代理或使用用户明确约定的配置，不为任务编造模型别名。没有独立分配的角色由主代理承担。

| 角色 | 主要工作与文件边界 |
|---|---|
| 主代理 / 集成人 | 主持 TP-L0.1，集成生产启动链、CI 和文档，核对实际 HEAD/tree 与证据；统筹任务和失败处理。 |
| CLI 作者 | 拥有 main.go/main_test.go 的参数与 usage 改动；按已冻结接口消费 launcher 结果，不重复实现 resolver。 |
| resolver / identity 作者 | 按文件划分 resolver 与 identity 组件；必要时可分别分配，但公共类型保持单一写入人。 |
| fixture 作者 | 拥有独立默认入口夹具及对应集成文件，准备实际安装布局与外部证据目录。 |
| 只读验收角色 | 对照设计、源码、测试断言和真实运行记录复核，关注假默认入口、身份绕过、预算与实际 Codex 状态，不在审阅时修改作者文件。 |

同一时段每个文件只有一名写入者。L1 仅在 L0 接口确定后并行；L2 装配由主代理指定单一作者。子代理返回具体文件、断言范围和未决项，不擅自远端推送、merge 或创建 tag；这些动作由主代理依据会话授权统一处理。

## 4. 验证命名与 CI 选择

基线 CI 用 `^TestRaw` 选择独立集成测试，用 `^TestRealGopls` 选择真实工具测试。默认入口新测试建议命名为 TestRawDefaultLauncher… 与 TestRealGoplsDefaultLauncher…；如果采用其他名称，必须同步选择器并从 go test JSON 核实实际执行。仅在仓库新增测试函数而 CI 未选中不算接入完成。

保留以下既有矩阵，不将全量协议套件缩减为启动测试：

| 平台 | Go | 必需自动化 |
|---|---|---|
| Linux | 1.25.x | CLI/定位/身份组件、backend/workspace 回归、raw 独立夹具，保留 race |
| Linux | 1.27.2 | 上述项目，以及 gopls v0.24.0 的真实互通 |
| macOS | 1.27.2 | 上述项目，以及 macOS 上的文件身份/符号链接/权限与 gopls v0.24.0 互通 |

实际 Codex 是单独门禁。CI 没有实际客户端或凭据时，可保留人工或独立环境执行入口和 pending 状态；不能把“编译、raw harness、真实 gopls 已通过”合并改写成“Codex 已通过”。平台结论逐项记录，macOS 通过不自动代表 Linux 的客户端行为，反之亦然。

## 5. 完成条件和失败处理

代码交付的最低要求是默认入口与显式入口都有可追溯的证据，且默认启动没有改变监测内容、协议恢复、请求不重放、清理预算与数据传递语义。出现预算或环境限制时，保留失败数据并说明实际边界；不得为满足 10s/60s 而跳过内容扫描、减少必须监测的输入、关闭 Guard 或降低恢复正确性。

“用户模板只改 command 即可用”的最终结论还要求 VAL-LAUNCH-034、035 在记录的平台、项目和实际 Codex 配置下通过。若默认 cwd 是 home/root，缺少可用 gopls，或项目所需输入在默认监测范围之外，应按既有约束报错或明确支持范围；不能从二进制安装目录推断项目，也不能悄悄补入 cwd、watch-root、GoDiscovery、args 或 timeout 来让目标模板通过。

已通过工程验证的入口代码可以单独合入；真实 Codex 证据缺失时，产品验收继续保持 pending/blocked，并在交付说明中明确区分。代码合入与“只改 command 已完全验收”是两个独立状态。

本文件与[验收矩阵](validation.md)只提出工作，不报告已实现、已运行或已发布。

