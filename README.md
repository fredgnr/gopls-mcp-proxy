# gopls-mcp-proxy

为 `gopls mcp` 提供稳定的 MCP stdio 连接。工作区的依赖或 Go 文件集合变化后，代理会关闭旧 gopls、重新初始化新后端，Codex 无需断开连接。

首版面向 **macOS / Linux、单工作区、单后端**。代理使用官方 MCP Go SDK，分别管理上下游会话。协议采用 `2025-11-25`，上游也接受更早的兼容版本；尚未开放 `2026-07-28` 的新会话语义。

## 安装与使用

构建代理需要 Go 1.25 或更新版本；还需要一个支持 `gopls mcp` 的 gopls。真实集成测试使用 **Go 1.27.2 + gopls v0.24.0**。gopls 自身的 Go 版本要求独立于代理。

```sh
go install github.com/fredgnr/gopls-mcp-proxy/cmd/gopls-mcp-proxy@latest
go install golang.org/x/tools/gopls@v0.24.0

gopls-mcp-proxy --gopls /absolute/path/to/gopls --workspace /absolute/path/to/project
```

程序启动后等待 stdin 上的 MCP 消息，**stdout 不输出日志**。帮助、版本信息、代理日志和 gopls 日志均写入 stderr。退出客户端或发送 SIGINT/SIGTERM 会清理后端。

Codex 配置示例（将路径换成自己的绝对路径）：

```toml
[mcp_servers.gopls]
command = "/Users/you/go/bin/gopls-mcp-proxy"
args = [
  "--gopls", "/Users/you/go/bin/gopls",
  "--workspace", "/Users/you/projects/my-project",
  "--debounce", "500ms",
  "--poll-interval", "2s",
  "--request-timeout", "110s"
]
cwd = "/Users/you/projects/my-project"
startup_timeout_sec = 60
tool_timeout_sec = 120
```

代理继承 `PATH`、`GOROOT`、`GOPROXY` 等环境。内部请求期限应小于客户端的工具超时。MCP 初始化成功前，代理先等待首个后端就绪。

## 哪些变化会重建

| 工作区变化 | 行为 |
|---|---|
| `go.mod`、`go.sum`、`go.work`、`go.work.sum` 内容变化、新增或删除 | 重建 |
| `.go` 文件新增、删除、重命名或所在目录移动 | 重建 |
| 已有 `.go` 文件仅编辑内容 | 不重建，由 gopls 文件监听更新分析 |
| 只有时间戳变化 | 不重建 |
| 原子保存后最终 `.go` 路径集合不变 | 安静窗口内合并事件，不重建 |
| 批量生成 Go 文件 | 文件稳定后重建 |

扫描覆盖配置目录、包含它的模块、当前生效的 `go.work` 中的 `use` 模块，以及通过 `--replace-dir` 明确添加的本地目录。`--replace-dir` 可重复，且相对路径以 workspace 为基准：

```sh
gopls-mcp-proxy --workspace /src/app --replace-dir ../shared --replace-dir ../api
```

外部 `replace` 目录**不会自动递归展开**，需要显式添加。自动发现 `go.work` 时，每次扫描都会重新检查祖先目录；支持 `GOWORK=off`、显式绝对路径以及 `go env -w` 持久化设置。祖先目录只检查依赖文件，不做无界递归扫描。

扫描排除 `.git`、`.hg`、`.svn`、`node_modules`、当前 `GOMODCACHE` 和 `GOROOT`，拒绝把文件系统根目录或用户主目录作为扫描根。目录符号链接不递归跟随；显式目录参数会解析符号链接。大工作区每次请求都要扫描，延迟取决于文件数量。

## 一致性与失败行为

- fsnotify 事件先暂停派发，默认等待 500ms 安静窗口，再校验 SHA-256 指纹。每 2 秒轮询兜底，每次派发前和返回前也做完整校验。
- 指纹包含依赖文件路径、存在状态、完整内容，以及排序后的 Go 文件路径；不会以 mtime 代替内容校验，也不会对普通 Go 文件计算内容哈希。
- 只有一个 supervisor 可以发布后端。重建先排空已有调用，再关闭旧进程；启动前后状态不一致的新进程会被丢弃。
- 排队、扫描和执行共享一个请求总期限。队列有上限，支持取消；已发送的工具调用不自动重放。
- 旧代次的结果会返回 `PROXY_STALE_RESULT`。依赖解析错误会保留文件路径和具体错误；修复文件后自动重试。
- gopls 崩溃或请求超时后会重建；启动失败使用有上限的指数退避。不会恢复使用已经失效的旧后端。
- 工具定义（含 schema、annotations）及结果的 `content`、`structuredContent`、`isError`、`_meta` 保留。进度 token 在下游会话内重新分配，取消由 SDK 传播。
- 工具清单固定在首个成功后端；重建后定义发生变化会要求重启代理会话。gopls 可执行文件内容变化也会被拒绝，需重启代理。
- `ping` 由代理本地处理，重建时仍可响应。仅声明 tools 能力，不代理 resources、prompts、sampling 或 elicitation。

保证以派发前和返回前的校验时刻为界，不提供文件系统事务快照。`go get` 分阶段改写依赖文件、两次写入间隔超过安静窗口时，可能出现多次有效重建。

## 参数

| 参数 | 默认值 | 含义 |
|---|---|---|
| `--gopls` | `gopls` | 后端可执行文件 |
| `--workspace` | `.` | 工作区目录 |
| `--replace-dir` | 无 | 额外本地依赖目录，可重复 |
| `--debounce` | `500ms` | 事件安静窗口 |
| `--poll-interval` | `2s` | 全量轮询周期 |
| `--drain-timeout` | `5s` | 等待正在执行的请求结束的上限 |
| `--startup-timeout` | `45s` | 后端初始化期限；也用于首个后端的总体等待 |
| `--shutdown-timeout` | `2s` | 关闭 stdin 后及 SIGTERM 后各自的等待时间 |
| `--request-timeout` | `110s` | 每次工具调用的总期限 |
| `--queue-limit` | `64` | 最多待派发请求数 |
| `--max-concurrent` | `8` | 最多并行执行请求数 |
| `--retry-min` / `--retry-max` | `1s` / `30s` | 启动失败退避范围 |
| `--version` | — | 输出版本并退出 |

常见错误：`PROXY_QUEUE_FULL`（队列满）、`PROXY_WAIT_TIMEOUT`（等待超时）、`PROXY_EXECUTION_TIMEOUT`（执行或排空期限已到）、`PROXY_WORKSPACE_ERROR`（扫描或依赖解析失败）、`PROXY_BACKEND_UNAVAILABLE`（后端不可用）、`PROXY_STALE_RESULT`（旧结果已丢弃）、`PROXY_SESSION_RESTART_REQUIRED`（工具定义变化）。这些代理错误使用 MCP `isError: true`，下游协议错误仍作为协议错误返回。

## 开发与验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/gopls-mcp-proxy

# 实际启动代理和 gopls，在同一个 MCP 会话中运行两次 go get 升级。
GOPLS_INTEGRATION=1 GOPLS_BIN="$(command -v gopls)" \
  go test -v ./integration -count=1
```

常规测试不下载 protobuf，使用隔离目录、受控后端和独立 MCP 子进程覆盖并发与异常场景。集成测试会联网下载 protobuf，验证 `v1.34.2 → v1.35.2 → v1.36.6` 的符号源码路径切换，并验证 gopls 自身监听到内容编辑且代理 generation 不变。

CI 在 Linux/macOS 上执行 race 检查、vet、构建及真实 gopls 验收，另检查代理的最低 Go 版本。工作流使用只读权限和 `persist-credentials: false`。

实现结构：

| 路径 | 职责 |
|---|---|
| `cmd/gopls-mcp-proxy` | 参数、Go 环境、信号和 stdio 入口 |
| `internal/workspace` | 范围发现、指纹、文件监听 |
| `internal/backend` | 下游 MCP、roots、进度映射、进程回收 |
| `internal/proxy` | 串行监督、有界排队、代次一致性、上游 MCP |
| `integration` | 真实代理与 gopls 验收 |

原始设计见 [docs/design.md](docs/design.md)，验证范围见 [docs/validation.md](docs/validation.md)。

参考：[gopls MCP](https://go.dev/gopls/features/mcp)、[MCP 生命周期](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle)、[roots](https://modelcontextprotocol.io/specification/2025-11-25/client/roots)、[stdio](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)、[官方 Go SDK](https://github.com/modelcontextprotocol/go-sdk)。
