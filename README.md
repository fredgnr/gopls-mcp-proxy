# gopls-mcp-proxy

给 MCP stdio 后端增加由代理负责的输入刷新。代理监测工作目录中的文件内容，输入变化后回收旧进程、启动新进程，并在协议状态可以安全恢复时保持客户端连接。

默认用法是包装 `gopls mcp`，也可以包装其他 MCP stdio 程序。代理不维护 gopls 工具清单，不调用 reload 工具，不依赖 gopls 自己发现文件变化。方法名、工具参数、schema、结果和上游错误通过原始 JSON 转发；只有协议关联字段在需要隔离请求时改写。

本轮范围是 **macOS / Linux、stdio、一个客户端、一个活跃后端**。监测多个目录不会启动多个后端。协议转发与跨进程恢复的范围分别说明在下文；当前提交的运行证据见[验证说明](docs/validation.md)，完整目标与验收编号见[改造方案](docs/changes/proxy-refresh-protocol/README.md)。

## 安装与运行

代理源码要求 Go 1.25 或更新版本。代理自身不执行 `go env`，包装非 Go 后端时也不要求安装 Go；运行 gopls 时仍需满足 gopls 自身的运行要求。以下 CLI 对应此源码版本；旧 Release 不会自动获得新能力。

```sh
go install github.com/fredgnr/gopls-mcp-proxy/cmd/gopls-mcp-proxy@main

cd /absolute/path/to/project
gopls-mcp-proxy
```

无参数，或只有合法代理选项时，默认启动 `gopls mcp`。请保留原生 gopls，把它与代理放在同一目录最直观；代理也可以从继承的 PATH 中查找。

```text
gopls-mcp-proxy [代理选项]
gopls-mcp-proxy [代理选项] -- 程序 [原始参数...]
```

默认查找以代理实际可执行文件的物理目录为准：通过符号链接启动时取链接目标目录；通过硬链接启动时取本次可获得的自身路径目录。只有同目录 gopls 确实不存在才搜索 PATH，并只考虑绝对目录。第一个存在的候选若是坏链接、目录、无执行位或指向代理自身，会明确失败，不静默换用其他版本。空 PATH 项、`.` 和相对目录不会参与默认搜索。

显式 `--` 后必须提供程序。其后的空参数、空格、中文、引号、`--help`、`--version` 和 shell 字符都按原始 argv 传入，不再由代理解析，也不拼接为 shell 命令。显式程序名与相对路径保持原解析规则，适合包装其他 MCP stdio 后端：

```sh
gopls-mcp-proxy -- /absolute/path/to/gopls mcp
```

工作目录和环境由调用方设置。安装目录不是项目目录；`--watch-root` 只增加输入范围，不切换 cwd，也不替客户端声明 MCP roots。默认不会自动监测 cwd 外的 replace/use 目录；需要这些输入时请添加 watch root。

程序定位路径在一次代理运行中固定为绝对路径，环境和参数也固定。每次启动前检查后端与代理自身的启动身份、当前文件身份，拒绝直接、符号链接或硬链接指向自身。原路径上的正常二进制替换或符号链接升级允许在下次已有刷新时生效；删除、损坏或改指自身会进入既有有界恢复，不重新搜索后端。该检查不消除文件检查到执行之间的竞态，也不识别任意包装脚本的间接循环。

帮助、版本、代理日志和后端 stderr 均写入 stderr；stdout 只输出 MCP 帧。客户端 EOF、SIGINT 和 SIGTERM 会触发后端清理。

### Codex 配置

没有 `args` 的 MCP 条目可以使用默认入口：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls-mcp-proxy"
```

该模板要求 Codex 实际从项目目录启动进程，原生 gopls/Go 已安装，所需输入位于默认监测范围内。工程测试与真实 Codex 的实际验收状态分别记录在[默认入口实施结果](docs/changes/default-gopls-launcher/implementation-results.md)。客户端默认超时与代理内部预算分别生效，未经验证的大项目不在保证范围内。

原生 gopls 的标准 MCP 配置是 `command=gopls`、`args=["mcp"]`。已有该配置时，改用代理默认入口还需要删除 `args`；保留裸 `mcp` 会得到迁移提示。显式通用入口也仍可用：

```toml
[mcp_servers.gopls]
command = "/Users/you/go/bin/gopls-mcp-proxy"
args = ["--watch-root", "/Users/you/projects/shared", "--", "/Users/you/go/bin/gopls", "mcp"]
cwd = "/Users/you/projects/my-project"
startup_timeout_sec = 60
tool_timeout_sec = 120
```

这份扩展配置明确增加了 cwd、输入范围和超时，不属于“只改 command”的配置验收。代理自己的版本仅用于诊断和打包，不会替换原生 `serverInfo` 或 `clientInfo`。

### 每次工具调用前重建 gopls

增加 `--refresh-mode=per-call` 后，每个独立的 `tools/call` 都会使用新启动的后端。即使文件没有变化，或变更位于代理监测范围之外，也不复用上一次工具调用的 gopls 进程：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls-mcp-proxy"
args = ["--refresh-mode", "per-call"]
enabled = true
```

工具调用排队执行；代理完成前一调用的交付和清理，再为下一调用启动后端并恢复允许的协议状态。工具调用执行期间，其他普通请求也会等待，取消、进度和反向请求响应仍与当前调用的后端绑定。`tools/list` 等其他方法不会单独要求重建；原有文件监测和内容检查继续生效，实际输入变化仍可能使正在执行的调用失效。调用的排队、重建与执行仍受总超时约束。

此模式要求将 `tools/call` 逐条发送。包含它的纯请求 batch 在派发前按原分组返回 `per_call_batch_unsupported`，并标记 `requestDispatched: false`；若同一 batch 还混有通知或响应，则在转发任何成员前结束连接。默认模式和不含 `tools/call` 的 batch 保持原有规则。

这个模式适合“保存文件后再查询”：新增、删除、重命名和内容修改由新进程重新加载，不要求代理先发现变化。实际可见范围仍由 gopls 的 cwd、Go 工作区和构建配置决定；编辑器未保存内容、调用期间的并发写入以及跨进程业务句柄不因此获得保证。已派发的工具调用不会自动重放。

默认模式是 `--refresh-mode=on-change`，仍按输入变化刷新。该选项需要包含此功能的新构建；已发布的 `v0.2.0` 不包含它。

### 调用结束后释放 gopls

`per-call` 决定每次工具调用是否使用新进程；`backend-lifecycle` 决定用完后是否保留进程。默认 `resident` 保留最近的后端，直到下一次刷新、故障或连接关闭。对有限 gopls 工具调用，可显式选择 `on-demand`：

```toml
[mcp_servers.gopls]
default_tools_approval_mode = "approve"
command = "/Users/you/go/bin/gopls-mcp-proxy"
args = [
  "--refresh-mode", "per-call",
  "--backend-lifecycle", "on-demand",
  "--backend-idle-timeout", "10s",
]
enabled = true
```

这些 `backend-*` 选项需要包含本功能的新构建，v0.6.0 不支持。`on-demand` 首版仅与 `per-call` 配合，其他组合会在启动后端前报配置错误。

有限工具调用的最终响应完整写入前端管道、相关交互排空且协议状态允许释放后，代理立即开始清理主后端，不等待 10 秒。前端 MCP 连接继续存在，下次需要后端处理的请求或通知会按需唤醒并恢复协议。初始化、`tools/list`、`ping` 等普通交互结束后，使用大于零的可配置空闲期限，默认 10 秒。`ping` 仍转发给真实后端；例如每 5 秒一次的心跳可能使普通请求的后端一直驻留。

未完成握手、分页、显式订阅、任务、多回合交互以及未知或不确定的协议状态会暂缓自愿释放。它们不取消下一次工具调用原有的强制刷新，也不提供新的跨进程业务句柄恢复保证。通用 MCP 后端依赖不透明进程内状态时，应保留 `resident`。

休眠时，文件变化只留待下次唤醒重新检查，不启动 gopls。无显式订阅时仅声明目录变更通知能力不会永久阻止休眠，因此休眠期间不保证实时目录推送；唤醒时按既有恢复规则校验目录并发送允许的失效通知。已按旧目录构造的请求不会被自动改写或重放。需要持续在线通知时使用 `resident`。

工具之后紧接普通请求可能增加一次冷启动；连续独立工具调用仍各自使用新后端。主 gopls 清理确认后才启动下一主后端，独立 session 的 telemetry 退出由测试观察器另外验证。此功能不会清理其他宿主直连的 gopls，也不修改用户 telemetry 配置或设置固定 `GOMEMLIMIT`。回退时改为 `--backend-lifecycle=resident` 并让宿主重新建立 MCP 连接即可保留原有 per-call 行为。实现边界及验收记录见[生命周期说明](docs/changes/on-demand-lifecycle.md)。

### 从旧命令迁移

旧的 `--gopls`、`--workspace`、`--replace-dir` 已移除；使用它们会返回迁移提示。

```sh
# 旧命令
gopls-mcp-proxy --workspace /src/app --gopls /opt/bin/gopls --replace-dir ../shared

# 新命令
cd /src/app
gopls-mcp-proxy --watch-root ../shared -- /opt/bin/gopls mcp
```

MCP 配置中将旧 `--workspace` 移到客户端的 `cwd` 配置，将额外目录改为 `--watch-root`，；需要指定其他后端时使用显式 `-- program argv`。

## 输入监测与刷新

默认扫描 cwd 和重复指定的 `--watch-root`，对所有纳入范围的常规文件计算完整内容 SHA-256，包括已有 `.go`、隐藏文件、vendor、嵌入资源和 C/C++ 输入。默认排除 `.git`、`.hg`、`.svn` 内部目录；`node_modules` 和生成目录不会自动排除，需要按实际输入范围配置。

```sh
cd /src/app
gopls-mcp-proxy \
  --watch-root ../shared \
  --exclude 'node_modules/**' \
  --exclude 'build/**' \
  -- /opt/bin/gopls mcp
```

`--include` 和 `--exclude` 可重复，采用相对各监测根的斜杠路径。`*`、`?`、`[]` 匹配单段，独立的 `**` 匹配零段或多段；没有 `/` 的模式匹配任意深度的名称。排除优先，不支持 `!` 取反、反斜杠或绝对 glob。将文件排除意味着代理不再保证它的变化触发刷新。

| 变化 | 行为 |
|---|---|
| 已有 Go 文件或其他输入内容改变 | 重建 |
| 文件新增、删除、重命名、目录移动 | 重建 |
| 同长度修改并恢复原 mtime | 仍能通过内容校验发现 |
| 原子替换，或已观察到写入后内容恢复原值 | 保守重建，事件失效 epoch 不回退 |
| 仅时间戳改变，内容和范围不变 | 不单独要求重建 |
| 所有监听事件丢失，但最终内容改变 | 周期扫描与请求前后扫描兜底 |

目录内部符号链接只纳入链接目标文本，不递归跟随；需要监测外部目标时显式添加 `--watch-root`。显式监测根可以是文件或目录，根本身为符号链接时每次扫描重新解析并记录目标变化。拒绝把整个文件系统根或用户主目录作为扫描根。

`--go-discovery` 可选启用 Go 工作区范围发现，读取祖先依赖文件和有效 `go.work` 的 `use` 目录；默认通用模式不解析 Go 语法。该选项读取当前进程的 `GOWORK`（`off` 或绝对路径），不执行 `go env`、不读取 `go env -w` 持久化设置，也不修改后端环境。外部本地 replace 目录应显式加入 `--watch-root`。发现解析失败时保留显式范围和最后成功的发现范围，并继续监测原始文件内容。

## 协议与恢复范围

原始消息通道保留未知字段、缺失与 null 的区别、数字精度、原生工具描述及错误。请求 ID 区分字符串和数字；反向请求、进度、取消和内部恢复请求按物理后端连接隔离。`ping` 转发给后端；代理不新增工具，也不伪装后端健康。

允许的 batch 作为整体接收和派发，不拆成多个执行批次。其请求成员数不得超过 `--max-concurrent`，并且必须同时满足队列数量、字节与帧预算；超限会在写入后端前拒绝。混有通知的 batch 无法为所有成员提供请求错误，因此预算拒绝会结束连接。`per-call` 模式额外拒绝含 `tools/call` 的 batch，具体行为见上文。

| 协议配置 | 稳定连接内 | 替换进程时 |
|---|---|---|
| `2024-11-05`、`2025-03-26`、`2025-06-18`、`2025-11-25` | 原生初始化、普通请求、通知、反向请求；batch 仅在 `2025-03-26` 启用 | 重建确认成功的标准初始化、日志级别及资源订阅；初始化约定和目录必须满足恢复检查 |
| `2026-07-28` | 保留每次请求的版本、能力、身份 metadata；转发 MRTR 和长期订阅 | 不强制 initialize/discover；长期订阅按协议结束，由客户端重新建立；不自动发起 MRTR 下一轮 |
| 未知修订或未知初始化状态扩展 | `on-change` 保留合法原始消息；`per-call` 在初始化时拒绝已知不可恢复声明 | 没有明确恢复规则时关闭连接并要求重连 |

表中描述已实现的 profile 和恢复策略，不代表任意客户端、任意后端或全部验收组合均通过。五修订 fixture 与真实 gopls 是两类独立证据；现代 fixture 通过不表示真实 gopls 支持现代协议。

初次连接将真实客户端握手原样发送给后端，不强制 `tools/list` 预热。`per-call` 每次工具调用都必须恢复初始化，因此遇到已知不可恢复的成功握手时，会直接向客户端返回 `protocol_restart_required` 并关闭连接，避免先报告就绪、再在第一次工具调用时失败。此时 `initialize` 已派发并收到后端响应，错误中对应的两个标记均为 `true`；后端原生初始化错误仍原样转发。`on-change` 保留首次握手透传行为，在真正需要刷新时拒绝不可恢复状态。刷新时比较版本、能力、身份、instructions 等初始化信息；已观察目录若支持合法变更通知，则发送失效通知，否则需要完整、可比较且相同的分页快照。目录不完整、未知扩展、恢复失败或约定变化会返回 `protocol_restart_required` 并结束 stdio，客户端需重新建立连接。不会继续使用已知陈旧后端。

`2025-06-18` 和 `2025-11-25` 均允许恢复空的 `elicitation`，以及其中值为空对象的 `form`、`url` 声明，支持 Codex `0.163.0` 的 June 初始化组合。代理保留客户端身份与能力内容，恢复时使用实际协商版本；这不升级协议，也不恢复正在进行的 elicitation 交互。未知模式、非空模式对象和错误类型仍不能恢复。

`codex/auth-change` 的客户端空标记 `experimental: {"codex/auth-change": {}}` 可以原样恢复。非空内部对象、错误类型、其他未知实验扩展，以及服务端声明的同名能力仍不能恢复。服务端声明会启用额外的认证变更通知，需要独立的状态恢复机制，不能只重放初始化。[此次故障的根因、兼容边界与验证方法](docs/changes/codex-auth-change-recovery.md)包含上游源码依据。

协商版本为 `2025-06-18` 或 `2025-11-25` 时，客户端的 `capabilities.extensions: {}` 也允许原样恢复：空映射没有声明具体扩展，因此不增加需要重建的扩展状态。这是一条显式的客户端兼容规则；字段省略与空对象都可恢复，但不会互相改写。当前没有支持的非空 `extensions` 声明；任何扩展键、`null`、数组及其他错误类型仍拒绝恢复，服务端声明及其他版本的规则不变。详见[空 extensions 修复说明](docs/changes/empty-extensions-recovery.md)。

收到后端成功握手后，会在 stderr 记录初始化声明的恢复资格，包含 `requested_version`、`negotiated_version`、`recoverable`；拒绝时另有 `side`、`path`、`reason`、`actual_type`，不记录完整初始化报文或被拒绝的字段值。恢复资格仅说明初始化声明可重建，后续状态与候选后端仍需通过一致性检查。已知不可恢复状态会在启动替代后端前拒绝。

分页 cursor、task ID、业务 handle 和 MRTR `requestState` 原样传递，但不保证跨进程有效。代理不重放创建这些状态的业务调用，也不把旧版反向请求翻译为现代 MRTR。

## 一致性与错误

文件事件、内容指纹和独立 dirty epoch 控制后端发布。每次派发和交付有限请求结果前进行内容检查，启动候选前后也复核输入；启动期间变化的候选不能成为新代次。刷新暂停新普通请求，已派发操作需要的反向回复、取消等控制消息继续有界转发。

已进入写入阶段的业务请求一律不自动重放，包括部分写入、崩溃、超时和旧代次结果。代理错误是 JSON-RPC 错误（应用 code `1000`），不是伪造的工具 `isError` 结果。错误 `data` 含 `reason`、`generation`、`requestDispatched`、`backendResponseReceived`、`sideEffects` 和 `automaticReplay: false`。上游错误保持原样。

恢复失败时，尚未派发的排队请求会收到 `protocol_restart_required`，其中 `requestDispatched: false`；若初始化声明被拒绝，`data.recovery` 同时提供字段路径与原因。代理最多等待 `cleanup-timeout` 与 `request-timeout` 中较短的时间写出这些错误，再关闭连接并清理候选后端。输出阻塞或超出预算时不保证错误一定送达。

这尤其影响写工具：工具可能已经修改文件，但结果因跨越刷新边界而被拒绝。`requestDispatched: true` 表示副作用不确定，使用方需要核对实际状态再决定下一步，不能把错误当作未执行证明。

保证以扫描检查点为界，不提供文件系统事务快照。完全发生在两次观察之间、且所有事件都丢失的短暂变化无法被证明发现。内容扫描和进程重建有实际成本；大目录可通过明确的输入范围控制开销。

## 预算与选项

| 参数 | 默认值 | 含义 |
|---|---|---|
| `--refresh-mode` | `on-change` | `on-change` 按输入变化刷新；`per-call` 为每个独立 `tools/call` 启动新后端 |
| `--backend-lifecycle` | `resident` | `resident` 保留后端；`on-demand` 安全完成后释放，首版要求 `per-call` |
| `--backend-idle-timeout` | `10s` | on-demand 普通交互后的空闲期限，必须大于零；工具完成释放不等待此期限 |
| `--watch-root` | 无 | 额外输入文件或目录，可重复；cwd 始终纳入 |
| `--include` / `--exclude` | 无 | 输入 glob，可重复 |
| `--go-discovery` | `false` | 可选 Go 工作区发现 |
| `--disable-watcher` | `false` | 只用轮询和请求检查，便于验证漏事件路径 |
| `--debounce` / `--max-debounce` | `500ms` / `2s` | 安静窗口和本轮最大合并等待 |
| `--poll-interval` | `2s` | 周期全量内容检查 |
| `--drain-timeout` | `5s` | 等待已派发有限请求结束的上限 |
| `--startup-timeout` | `45s` | 单次启动及协议恢复上限 |
| `--shutdown-timeout` | `2s` | 关闭 stdin 后、TERM 后各自等待上限 |
| `--refresh-timeout` | `60s` | 一轮恢复预算；on-demand 新需求唤醒时重新计时，休眠不消耗预算 |
| `--cleanup-timeout` | `6s` | 最后资源清理预算 |
| `--request-timeout` | `110s` | 有限请求排队、扫描、重建、执行的总期限 |
| `--queue-limit` | `64` | 待派发请求数，batch 按请求成员计数 |
| `--queue-bytes` | `67108864` | 字节预算，64 MiB |
| `--max-frame-bytes` | `16777216` | 单帧上限，16 MiB |
| `--max-concurrent` | `8` | 普通有限请求并发上限；`per-call` 工具调用始终串行 |
| `--retry-min` / `--retry-max` | `1s` / `30s` | 有上限的恢复退避 |
| `--help` / `--version` | — | 输出代理帮助或版本后退出 |

确认建立的现代长期订阅不使用普通请求期限，也不阻塞 drain。通知不能返回请求错误；无法在预算内转发时结束连接，不静默丢弃。清理按关闭 stdin、TERM、KILL、Wait 执行，覆盖 Unix 进程组；未确认完成清理时不启动下一后端。默认正常恢复预算为 60s，最后清理最多 6s；恢复拒绝的错误交付分支还可能增加最多 6s 的输出等待。这些预算不保证一定恢复服务。

## 开发与验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/gopls-mcp-proxy

# 必跑独立后端：不需要 gopls，也不依赖后端自行刷新。
go test -race -v ./integration -run '^TestRaw' -count=1

# 真实 gopls 互通：联网下载测试依赖。
GOPLS_INTEGRATION=1 GOPLS_EXPECTED_VERSION=v0.24.0 GOPLS_BIN="$(command -v gopls)" \
  go test -v ./integration -run '^TestRealGopls' -count=1
```

独立后端只在启动时读取一次输入，用同 PID 的负向对照证明它不会自行刷新，再验证相同代理进程/前端连接内的后端换代。真实 gopls 测试执行 protobuf `v1.34.2 → v1.35.2 → v1.36.6`，并要求内容编辑也由代理重建。

真实后端矩阵固定覆盖 gopls `v0.23.0` 与 `v0.24.0`。本地测试前将 `GOPLS_EXPECTED_VERSION` 设为所安装的其中一个版本。Codex 初始化 fixture 根据上游实现与故障复现重建，覆盖两种刷新模式及符号增删改；它不代替用户实际 Codex 连接的最终验收。

CI 分别执行组件 race 测试、必跑 raw 协议/刷新测试、真实 gopls 互通和二进制构建，保存 JSON 测试日志。Linux/macOS 及最低 Go 版本矩阵以当前提交的 Actions 结果为准；设计用例尚未覆盖的组合不会因某个组件测试通过就被记为通过。详见[验证范围](docs/validation.md)和[实现设计](docs/design.md)。

## 二进制与发布

[GitHub Releases](https://github.com/fredgnr/gopls-mcp-proxy/releases) 的构建目标为 `darwin_arm64`、`darwin_amd64`、`linux_amd64`、`linux_arm64`。每个包包含代理、README 和 `BUILDINFO`，同时提供 `checksums.txt`。macOS 可用 `shasum -a 256`、Linux 可用 `sha256sum` 核对下载包。包内不包含 gopls。使用默认入口时将原生 gopls 保留在代理旁边，或放在 PATH 的绝对目录中；不要把代理覆盖为原生 gopls 文件。

版本 tag 的发布流程先完成 CI 和四目标构建，再创建 draft、上传附件并公开；已公开版本不覆盖。普通分支和 PR 只生成保留 14 天的 `release-dist` artifact。验证和构建使用只读权限，checkout 保持 `persist-credentials: false`；只有现有 tag 发布任务具有发布权限。本次改造不自动创建 tag 或 Release。

在 Linux 本地验证打包（需要空 `dist/`、Go、GNU tar、gzip、sha256sum）：

```sh
bash scripts/build-release.sh dev-$(git rev-parse --short=12 HEAD)
```

四目标采用 `CGO_ENABLED=0` 交叉编译；宿主二进制实际执行 `--help` 和 `--version`，交叉编译成功不等于另外三种架构都完成运行回归。
