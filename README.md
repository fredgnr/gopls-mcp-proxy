# gopls-mcp-proxy

给 MCP stdio 后端增加由代理负责的输入刷新。代理监测工作目录中的文件内容，输入变化后回收旧进程、启动新进程，并在协议状态可以安全恢复时保持客户端连接。

默认用法是包装 `gopls mcp`，也可以包装其他 MCP stdio 程序。代理不维护 gopls 工具清单，不调用 reload 工具，不依赖 gopls 自己发现文件变化。方法名、工具参数、schema、结果和上游错误通过原始 JSON 转发；只有协议关联字段在需要隔离请求时改写。

本轮范围是 **macOS / Linux、stdio、一个客户端、一个活跃后端**。监测多个目录不会启动多个后端。协议转发与跨进程恢复的范围分别说明在下文；当前提交的运行证据见[验证说明](docs/validation.md)，完整目标与验收编号见[改造方案](docs/changes/proxy-refresh-protocol/README.md)。

## 安装与运行

代理源码要求 Go 1.25 或更新版本。代理自身不执行 `go env`，包装非 Go 后端时也不要求安装 Go；运行 gopls 时仍需满足 gopls 自身的运行要求。以下新 CLI 对应此源码版本；此前已发布包请参照其随包 README，不能假定旧 tag 包含本次改造。

```sh
go install github.com/fredgnr/gopls-mcp-proxy/cmd/gopls-mcp-proxy@main

cd /absolute/path/to/project
gopls-mcp-proxy -- /absolute/path/to/gopls mcp
```

命令边界固定为：

```text
gopls-mcp-proxy [代理选项] -- 程序 [原始参数...]
```

`--` 后必须提供程序。其后的空参数、空格、中文、引号、`--help`、`--version` 和 shell 字符都按原始 argv 传入，不再由代理解析，也不拼接为 shell 命令。程序需要提供 MCP stdio；普通文本 stdout 子命令不属于协议代理的支持范围。

工作目录和环境由调用方设置。`--watch-root` 只增加输入监测范围，不切换 cwd，也不替客户端声明 MCP roots。程序路径通过启动时的 PATH 或显式路径解析，并固定为绝对定位路径；环境和参数在一次代理运行期间保持不变。下一次受控启动会读取该路径的新映像，允许符号链接目标或二进制被升级；代理不会单独监听二进制升级事件。

帮助、版本、代理日志和后端 stderr 均写入 stderr；stdout 只输出 MCP 帧。客户端 EOF、SIGINT 和 SIGTERM 会触发后端清理。

Codex 配置示例：

```toml
[mcp_servers.gopls]
command = "/Users/you/go/bin/gopls-mcp-proxy"
args = [
  "--watch-root", "/Users/you/projects/shared",
  "--debounce", "500ms",
  "--request-timeout", "110s",
  "--", "/Users/you/go/bin/gopls", "mcp"
]
cwd = "/Users/you/projects/my-project"
startup_timeout_sec = 60
tool_timeout_sec = 120
```

客户端超时应留出代理排队、扫描和恢复的时间。代理自己的版本仅用于诊断和打包，不会替换原生 `serverInfo` 或 `clientInfo`。

### 默认启动入口提案

让没有 `args` 的 Codex 配置只修改 `command`，由代理自动定位并启动 `gopls mcp`，见[默认 gopls 启动方案](docs/changes/default-gopls-launcher/README.md)。该方案状态为 **Proposed**；当前版本仍需按上述显式形式指定后端，新的默认入口与 Codex 验收尚未实施。

### 从旧命令迁移

旧的 `--gopls`、`--workspace`、`--replace-dir` 已移除；使用它们会返回迁移提示。

```sh
# 旧命令
gopls-mcp-proxy --workspace /src/app --gopls /opt/bin/gopls --replace-dir ../shared

# 新命令
cd /src/app
gopls-mcp-proxy --watch-root ../shared -- /opt/bin/gopls mcp
```

MCP 配置中将旧 `--workspace` 移到客户端的 `cwd` 配置，将额外目录改为 `--watch-root`，并显式写出后端程序及 `mcp` 参数。

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

一个 batch 作为整体接收和派发，不拆成多个执行批次。其请求成员数不得超过 `--max-concurrent`，并且必须同时满足队列数量、字节与帧预算；超限会在写入后端前拒绝。混有通知的 batch 无法为所有成员提供请求错误，因此预算拒绝会结束连接。

| 协议配置 | 稳定连接内 | 替换进程时 |
|---|---|---|
| `2024-11-05`、`2025-03-26`、`2025-06-18`、`2025-11-25` | 原生初始化、普通请求、通知、反向请求；batch 仅在 `2025-03-26` 启用 | 重建确认成功的标准初始化、日志级别及资源订阅；初始化约定和目录必须满足恢复检查 |
| `2026-07-28` | 保留每次请求的版本、能力、身份 metadata；转发 MRTR 和长期订阅 | 不强制 initialize/discover；长期订阅按协议结束，由客户端重新建立；不自动发起 MRTR 下一轮 |
| 未知修订或未知初始化状态扩展 | 合法原始消息可转发，仍受 framing 和资源预算限制 | 没有明确恢复规则时关闭连接并要求重连 |

表中描述已实现的 profile 和恢复策略，不代表任意客户端、任意后端或全部验收组合均通过。五修订 fixture 与真实 gopls 是两类独立证据；现代 fixture 通过不表示真实 gopls 支持现代协议。

初次连接直接转发真实客户端握手，不强制 `tools/list` 预热。刷新时比较版本、能力、身份、instructions 等初始化信息；已观察目录若支持合法变更通知，则发送失效通知，否则需要完整、可比较且相同的分页快照。目录不完整、未知扩展、恢复失败或约定变化会返回 `protocol_restart_required` 并结束 stdio，客户端需重新建立连接。不会继续使用已知陈旧后端。

分页 cursor、task ID、业务 handle 和 MRTR `requestState` 原样传递，但不保证跨进程有效。代理不重放创建这些状态的业务调用，也不把旧版反向请求翻译为现代 MRTR。

## 一致性与错误

文件事件、内容指纹和独立 dirty epoch 控制后端发布。每次派发和交付有限请求结果前进行内容检查，启动候选前后也复核输入；启动期间变化的候选不能成为新代次。刷新暂停新普通请求，已派发操作需要的反向回复、取消等控制消息继续有界转发。

已进入写入阶段的业务请求一律不自动重放，包括部分写入、崩溃、超时和旧代次结果。代理错误是 JSON-RPC 错误（应用 code `1000`），不是伪造的工具 `isError` 结果。错误 `data` 含 `reason`、`generation`、`requestDispatched`、`backendResponseReceived`、`sideEffects` 和 `automaticReplay: false`。上游错误保持原样。

这尤其影响写工具：工具可能已经修改文件，但结果因跨越刷新边界而被拒绝。`requestDispatched: true` 表示副作用不确定，使用方需要核对实际状态再决定下一步，不能把错误当作未执行证明。

保证以扫描检查点为界，不提供文件系统事务快照。完全发生在两次观察之间、且所有事件都丢失的短暂变化无法被证明发现。内容扫描和进程重建有实际成本；大目录可通过明确的输入范围控制开销。

## 预算与选项

| 参数 | 默认值 | 含义 |
|---|---|---|
| `--watch-root` | 无 | 额外输入文件或目录，可重复；cwd 始终纳入 |
| `--include` / `--exclude` | 无 | 输入 glob，可重复 |
| `--go-discovery` | `false` | 可选 Go 工作区发现 |
| `--disable-watcher` | `false` | 只用轮询和请求检查，便于验证漏事件路径 |
| `--debounce` / `--max-debounce` | `500ms` / `2s` | 安静窗口和本轮最大合并等待 |
| `--poll-interval` | `2s` | 周期全量内容检查 |
| `--drain-timeout` | `5s` | 等待已派发有限请求结束的上限 |
| `--startup-timeout` | `45s` | 单次启动及协议恢复上限 |
| `--shutdown-timeout` | `2s` | 关闭 stdin 后、TERM 后各自等待上限 |
| `--refresh-timeout` | `60s` | 从首个确认失效开始的一轮恢复预算 |
| `--cleanup-timeout` | `6s` | 最后资源清理预算 |
| `--request-timeout` | `110s` | 有限请求排队、扫描、执行的总期限 |
| `--queue-limit` | `64` | 待派发请求数，batch 按请求成员计数 |
| `--queue-bytes` | `67108864` | 字节预算，64 MiB |
| `--max-frame-bytes` | `16777216` | 单帧上限，16 MiB |
| `--max-concurrent` | `8` | 普通有限请求并发上限 |
| `--retry-min` / `--retry-max` | `1s` / `30s` | 有上限的恢复退避 |
| `--help` / `--version` | — | 输出代理帮助或版本后退出 |

确认建立的现代长期订阅不使用普通请求期限，也不阻塞 drain。通知不能返回请求错误；无法在预算内转发时结束连接，不静默丢弃。清理按关闭 stdin、TERM、KILL、Wait 执行，覆盖 Unix 进程组；未确认完成清理时不启动下一后端。默认故障收敛上限是正常恢复 60s 加最后清理最多 6s，不是保证 60s 内一定恢复服务。

## 开发与验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/gopls-mcp-proxy

# 必跑独立后端：不需要 gopls，也不依赖后端自行刷新。
go test -race -v ./integration -run '^TestRaw' -count=1

# 真实 gopls 互通：联网下载测试依赖。
GOPLS_INTEGRATION=1 GOPLS_BIN="$(command -v gopls)" \
  go test -v ./integration -run '^TestRealGopls' -count=1
```

独立后端只在启动时读取一次输入，用同 PID 的负向对照证明它不会自行刷新，再验证相同代理进程/前端连接内的后端换代。真实 gopls 测试执行 protobuf `v1.34.2 → v1.35.2 → v1.36.6`，并要求内容编辑也由代理重建。

CI 分别执行组件 race 测试、必跑 raw 协议/刷新测试、真实 gopls 互通和二进制构建，保存 JSON 测试日志。Linux/macOS 及最低 Go 版本矩阵以当前提交的 Actions 结果为准；设计用例尚未覆盖的组合不会因某个组件测试通过就被记为通过。详见[验证范围](docs/validation.md)和[实现设计](docs/design.md)。

## 二进制与发布

[GitHub Releases](https://github.com/fredgnr/gopls-mcp-proxy/releases) 的构建目标为 `darwin_arm64`、`darwin_amd64`、`linux_amd64`、`linux_arm64`。每个包包含代理、README 和 `BUILDINFO`，同时提供 `checksums.txt`。macOS 可用 `shasum -a 256`、Linux 可用 `sha256sum` 核对下载包。包内不包含 gopls。

版本 tag 的发布流程先完成 CI 和四目标构建，再创建 draft、上传附件并公开；已公开版本不覆盖。普通分支和 PR 只生成保留 14 天的 `release-dist` artifact。验证和构建使用只读权限，checkout 保持 `persist-credentials: false`；只有现有 tag 发布任务具有发布权限。本次改造不自动创建 tag 或 Release。

在 Linux 本地验证打包（需要空 `dist/`、Go、GNU tar、gzip、sha256sum）：

```sh
bash scripts/build-release.sh dev-$(git rev-parse --short=12 HEAD)
```

四目标采用 `CGO_ENABLED=0` 交叉编译；宿主二进制实际执行 `--help` 和 `--version`，交叉编译成功不等于另外三种架构都完成运行回归。
