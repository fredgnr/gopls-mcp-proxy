# 验收计划

状态：本文件全部用例为 **pending / 尚未执行**。本次文档评审和原有 CI 的通过，不代表这些新行为已经通过。设计依据见 [技术设计](design.md)，任务映射见 [实施计划](implementation-plan.md)。

## 1. 证据原则

核心结论必须能排除“后端自己刷新了”的解释。只看新结果、backend ready 日志或启动次数都不充分，应关联以下证据：

- 同一个 proxy PID 和前端连接标识。
- 每次 spawn 独立的 backend_connection_id / boot_id、真实子进程 PID、发布 generation。
- 输入变更前后路径/存在性/内容摘要，以及 proxy 的 dirty epoch 和扫描检查点。
- 请求接收、开始写入、后端确认接收、响应、取消与终止记录。
- 原始协议 transcript、实际修订、能力和恢复路径。

进程失败候选即使没有发布 generation，也必须拥有独立连接身份。所有日志时间使用单调时间差计算预算，墙钟时间仅用于定位。

每个用例记录实际命令、提交 SHA、Go/gopls 版本或 fixture 版本、OS/架构、结果、未覆盖限制。失败用例保留随机 seed 和事件顺序，不能用增加 sleep 来掩盖竞态。

阶段子断言与完整集成结果分别记录。P1 的固定连接测试、P2 的指纹/扫描测试可以先通过，但同一 VAL 中的跨刷新、现代协议或真实 gopls 断言仍保持 pending，不能用局部结果解锁一个依赖后续实现的完整门禁。阶段范围见实施计划第 1 节。

## 2. 无自刷新受控后端

建立独立 raw stdio helper：

1. 只在启动时读取测试清单中的输入，保存字节和摘要，记录 boot_id/PID/读取次数。
2. 后续请求只读取启动快照；不监听文件、不再次读输入、不运行 go 命令、不启动任何刷新线程。
3. 使用测试专用控制通道触发延迟、崩溃、半帧、反向请求、进度、MRTR 与长期订阅。
4. 使用独立 raw 客户端发送消息，避免两端共享同一个强类型 SDK 而同时丢弃未知字段。
5. 执行计数、控制文件和 transcript 默认置于监测范围外；测试“工具自身写入”时才明确写到被监测路径。

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-FIX-001 | 直接连接 helper：启动读 A，磁盘改 B，同 PID 再查询 | 仍返回 A，输入读取计数不增加；新启动 helper 才返回 B |
| VAL-FIX-002 | 原始客户端发未知字段、反向消息和预设错误 | transcript 精确保留测试载荷；不依赖 MCP SDK 归一化消息 |

先通过夹具自证，再运行 proxy 的刷新用例。真实 gopls 集成作为另一类互通证据保留。

## 3. CLI 与普通消息

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-CLI-001 | 分隔符后包含未知参数、空参数、空格、中文、引号和字面 shell 字符 | helper 收到的 program/argv 与直接 exec 对照一致，无 shell 展开和参数重排 |
| VAL-CLI-002 | 自定义 cwd/env/PATH/GOWORK/GOENV，监测根位于其他目录 | 后端工作目录和环境不被 watch-root 改写；监测与 MCP roots 独立 |
| VAL-CLI-003 | 分隔符前后分别使用 --help/--version | 前者查询 proxy，后者原样到 fixture；产品不按 gopls 子命令猜行为 |
| VAL-CLI-004 | 缺 program、混用旧参数、非 MCP 子程序输出、客户端 EOF | 配置或 transport 错误明确、有界回收；不把非 MCP stdout 子命令宣称为代理能力 |
| VAL-RAW-001 | 稳定输入下未知业务方法，包含 params/result/null/缺省字段 | 除记录在案的关联改写外，消息正文保留；不需要工具白名单 |
| VAL-RAW-002 | JSON-RPC error.data、工具 isError、未知 _meta、未知顶层字段 | 结果和错误分类及内容不被改写为 proxy 的工具错误 |
| VAL-RAW-003 | 复杂输入/输出 schema、annotations、instructions、未知 content 和结构化结果 | 不要求 inputSchema 固定为某种旧结构，不丢原始字段或新增结果类型 |
| VAL-RAW-004 | 半帧、多帧、短读、短写、EOF、无效 JSON | 分帧正确、同方向消息不交错；失败不会把半帧转成完整成功 |
| VAL-RAW-005 | 超过常见小缓冲的合法消息，以及超出配置上限的帧 | 合法大消息完整通过；真正超限明确失败，不截断、不无限分配 |

需要 byte-equality 的检查必须列明允许改写的具体 envelope/关联字段；不能在比较前删除所有未知字段或整个 _meta 来制造通过。

## 4. 输入检测与代次

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-INPUT-001 | 已有 .go 文件内容 A→B，路径不变 | proxy 检测变化；旧 helper 始终只知 A；新 PID/boot_id 返回 B |
| VAL-INPUT-002 | .go 与依赖文件修改后恢复原 mtime，且字节长度相同 | 内容指纹仍改变，不以 mtime/size 缓存作为真值 |
| VAL-INPUT-003 | 新增空文件、删除、改名、目录移动、文件/链接类型变化 | 路径/存在性/类型/内容变化进入指纹，下一代快照对应新输入 |
| VAL-INPUT-004 | 原子保存不同内容；同内容重写；已观察到的 A→B→A；纯元数据事件 | 不同内容必须重建；相关写入即使最终同 hash 也合并重建；纯元数据且内容不变可不重建 |
| VAL-INPUT-005 | 四类依赖文件连续两次变化；空文件新增/删除 | 两次都由 proxy 刷新；前端连接在已支持恢复条件下保持 |
| VAL-INPUT-006 | 外部显式根、Go 发现根增删、嵌入资源、vendor 和 cgo 输入 | 配置内输入都参与内容检测；监测范围变化进入指纹 |
| VAL-INPUT-007 | watcher 完全禁用或丢弃所有通知 | 请求派发前校验发现修改；不会依赖 helper 或 gopls 自刷 |
| VAL-INPUT-008 | 没有客户端业务请求，仅周期扫描；watcher 溢出/注册失败 | 空闲时也能发现变化并安排恢复；watcher 故障不关闭轮询 |
| VAL-INPUT-009 | 300 个文件生成、重复/乱序事件、持续写入 | 有限批次合并；首个失效 deadline/max-debounce 不重置；旧后端不能无限等待安静 |
| VAL-INPUT-010 | 在扫描、启动前后、派发前后、结果交付前注入变化 | 指纹和 epoch 检查均生效；过期候选不发布，不能仅依据扫描前旧引用 |
| VAL-INPUT-011 | 通用范围内出现未知或暂时非法 go.mod/go.work 字节 | 指纹仍可计算并转发给能处理它的 helper；proxy 不抢先执行 Go 语法验收 |
| VAL-INPUT-012 | 必需根不可读、历史派生根 ENOENT、发现失败后根恢复、符号链接改向 | 必需范围读取失败不宣称 fresh；历史缺失按设计记录与监测，不假装权限错误为不存在 |
| VAL-GEN-001 | 同一 proxy 内多轮内容变化 | 只有单活跃后端；每次候选 connection_id 唯一，已发布 generation 递增 |
| VAL-GEN-002 | 候选 A 失败，候选 B 尝试相同目标 generation，A 消息迟到 | A 的 initialize/反向响应/reader 事件不能影响 B 的 ready 或请求 |
| VAL-GEN-003 | 扫描完成与原子发布之间出现新 dirty 事件 | 发布/派发重新验证 epoch，旧候选不会被短暂发布 |
| VAL-GEN-004 | 可执行路径指向的程序或 symlink 目标在下一次已有刷新前更新 | 新启动采用该路径当前目标；协议兼容才继续；不要求单纯二进制变化即时刷新 |

完全发生在两个观察点之间且所有事件均丢失的 A→B→A 不是可证明测试目标。它作为限制写入报告，不能通过单纯最终 hash 宣称检测到了所有历史变化。

## 5. 具体协议修订

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-MCP-001 | 对每个声明支持修订执行首次连接和刷新 | P3 分别验收四个 legacy；P4 增加 2026-07-28；结果逐修订记录 |
| VAL-MCP-002 | legacy 真实 clientInfo/capabilities/未知初始化字段；恢复期 roots/sampling/elicitation | 原始上下文到上游；合法回调去真实客户端；内部结果/进度不泄露；恢复超时可中断 |
| VAL-MCP-003 | 新后端不接受旧协商版本，能力或只能初始化更新的信息不兼容 | 明确重连，不悄悄改变前端会话约定 |
| VAL-MCP-004 | 2025-03-26 混合 batch、分组响应、取消全部未发送成员 | 成员关联正确；通知无响应；全取消不发送空数组；后续修订不宣称 batch 标准能力 |
| VAL-MCP-005 | 现代首条请求直接为携合法 metadata 的普通 RPC | 正常转发，不强制 initialize 或 server/discover |
| VAL-MCP-006 | 连续现代请求使用不同能力、clientInfo 与未知 _meta | 每请求独立保留，不套用首次快照 |
| VAL-MCP-007 | 双代 discovery 成功、现代版本不支持、旧版不同错误码或超时 | 保留真实客户端可据以按规范选择/回退的结果；不只处理一个固定旧版错误 |
| VAL-MCP-008 | 现代上游反向交互 | 通过原生 MRTR；不生成旧式 server→client JSON-RPC request |
| VAL-MCP-009 | 未知修订在相同 framing 下正常通信，然后触发刷新 | 原始内容按声明转发；未知恢复规则时清理陈旧服务并重连，不猜握手 |
| VAL-MCP-010 | 目录通知可用/不可用；无通知时相同/改变/缺页/重复 cursor/未知字段快照；旧快照不对应客户端已见视图 | 合法通知或与已交付视图一致的完整相同比较才隐藏恢复；变化或无法证明时重连，不用事后旧后端枚举冒充历史 |

2024-11-05 不把后来新增的 batch 作为本轮标准承诺。2025-03-26 的 batch 专项不能因为其他版本单对象成功而跳过。

## 6. ID、取消与执行次数

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-ID-001 | 同时使用数值 1、字符串 "1" 和大整数 ID | 类型和精度保留，不发生 float64 键碰撞 |
| VAL-ID-002 | legacy 各后端都从反向 ID 1 开始；正反向同 ID；恢复控制请求冲突 | 前端旧 session 不复用反向 ID；响应回正确 connection_id；内部 ID 有隔离 |
| VAL-ID-003 | 现代同连接完成后正常复用 ID；前端超时但后端未结束时再次复用 ID | 正常复用通过；有歧义时先隔离/重建再派发，旧迟到响应不能完成新请求 |
| VAL-ID-004 | 进度 token 与 request ID 不同；旧进度迟到；token 再利用 | 独立关联，无歧义路由；必要 tombstone/连接隔离生效，不猜测错误上游的归属 |
| VAL-CANCEL-001 | 取消排队、等待容量或等待恢复的请求 | 队列计数/字节及时释放；不进入新后端 |
| VAL-CANCEL-002 | drain 中旧后端等待真实客户端反向回复，随后取消 | 回应和取消可到旧连接；不会被新业务闸门阻塞 |
| VAL-CANCEL-003 | 取消与响应竞态；请求已取消后再到结果 | 按该修订最多一次逻辑终结，不额外产生被禁止的响应 |
| VAL-CANCEL-004 | 持续进度、长反向操作、人类输入不结束 | drain 与总请求期限不重置；背压下也能最终清理 |
| VAL-NOREPLAY-001 | helper 先增加执行计数再阻塞，随后输入变化 | 该业务请求执行次数始终至多 1；新后端不收到自动复制调用 |
| VAL-NOREPLAY-002 | 已接收业务后崩溃、响应丢失 | 报告不确定/失败，不重新派发该业务请求 |
| VAL-NOREPLAY-003 | 部分写入、write 返回错误与记账竞态 | 第一次不可撤回写入前已有归属；无法证明未发送时不当成 queued |
| VAL-NOREPLAY-004 | batch 已开始写入，部分成员有结果，随后刷新/崩溃 | 可能发送的成员都不自动重放；通知无伪造响应；仍遵守批量响应约定 |

执行次数用 helper 的独立接收/执行记录证明，不能仅检查 proxy 内“retry=false”变量或日志。

## 7. 长期状态与错误

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-STATE-001 | legacy subscribe 成功；刷新；unsubscribe 与恢复交错 | 仅恢复仍有效订阅；重订后合法失效促使重读；已取消订阅不复活，不承诺事件无损 |
| VAL-STATE-002 | MRTR 返回 input_required，客户端稍后用新 ID 发 continuation | token 和输入原样；只有真实客户端发起下一轮；proxy 不自行调用业务 |
| VAL-STATE-003 | MRTR token 跨进程后被上游拒绝 | 保留真实错误，不解析/修补 token，不从头重做 |
| VAL-STATE-004 | 多条现代订阅，ack/通知交错，completion/cancellation/刷新交错 | ack 不作为最终响应清除 pending；按 subscriptionId 分流；有界结束、至多一次终止，无幽灵恢复 |
| VAL-STATE-005 | 返回 taskId、cursor 或未知工具 handle 后刷新 | 不伪造迁移、任务成功或终态；由真实上游决定有效性 |
| VAL-STATE-006 | 未知初始化一次性扩展、未知目录能力或不可确认恢复状态 | 不把 activeRPC=0 当作可无损迁移证明；按公开失败/重连边界结束 |
| VAL-STATE-007 | 支持 logging/setLevel 的 legacy 修订：成功、失败、取消及并发更新后刷新 | 仅恢复最后已确认值且确认顺序明确；不复活失败/不确定更新；内部消息不泄露，恢复失败明确重连 |
| VAL-ERROR-001 | 稳定代次的上游 JSON-RPC 错误和合法工具错误 | 原样传递，未被 proxy 自定义错误取代 |
| VAL-ERROR-002 | 请求从未写入后端即排队/扫描超时 | requestDispatched=false，原因清楚 |
| VAL-ERROR-003 | 合法工具写入监测文件后返回；或已派发跨刷新却未返回 | 明确实际派发/收到响应与否、副作用未知；没有“安全重试”提示或自动重放 |
| VAL-ERROR-004 | Go 语法错误与文件读取权限错误分别发生 | 后端业务诊断与 proxy 一致性错误来源区分；数字错误码满足所选修订规则 |

标准初始化、订阅和协议设置恢复必须在白名单内另有证据，不允许借此扩大为业务历史回放。

## 8. 生命周期、真实 gopls 与性能

| 编号 | 场景 | 必须结果 |
|---|---|---|
| VAL-LIFE-001 | 初始化不回复、忽略 TERM、管道不关闭、预算用尽 | EOF/TERM/KILL/Wait 在规定恢复及清理预算内收敛；无法确认回收就停止后续启动 |
| VAL-LIFE-002 | 子进程派生后代、候选淘汰、崩溃、客户端 EOF | Linux/macOS 管理的进程组无残留；回收状态有证据 |
| VAL-LIFE-003 | 后端不读 stdin、客户端不读 stdout、通知与队列饱和 | 帧/队列/控制流占用有界；不静默丢必要通知，不向通知回复错误；关闭可解除阻塞 |
| VAL-LIFE-004 | 固定 seed 的多轮重建/取消/崩溃，包含 100 轮压力场景 | 请求映射、tombstone、队列和进程句柄收敛；不依赖脆弱的少量 heap 波动阈值 |
| VAL-GOPLS-001 | 同一前端中 protobuf v1.34.2→v1.35.2→v1.36.6 | 原生 go_file_context 的声明路径进入对应版本；proxy PID/连接保持，后端换代 |
| VAL-GOPLS-002 | 真实 .go 内容编辑、新增/删除/原子保存 | 新后端 PID/boot_id 与新结果对应；删除 generation 不变的旧断言 |
| VAL-GOPLS-003 | 相同 workspace/cwd/env 下直连与代理的稳定调用对照 | 工具目录、参数、结果及协议行为按允许差异比较；不为通过而裁剪未知字段 |
| VAL-CI-001 | 最低 Go、Linux/macOS 核心门禁和固定 gopls 互通 | mandatory helper 不因缺 gopls 被 skip；真实集成单独记录；没有 continue-on-error 掩盖失败 |
| VAL-PERF-001 | 固定小/中/大目录及字节量，比较原始转发、内容校验与刷新 | 给出扫描/额外请求延迟/就绪耗时分布、总字节、文件数、硬件和缓存条件 |
| VAL-PERF-002 | 大文件流式扫描及反复重建 | 报告读取/哈希与内存成本、重建次数、资源收敛；不通过静默排除输入优化数据 |

真实集成沿用基线 CI 的 Go 1.27.2 与 gopls v0.24.0 作为可复现起点；代理最低 Go 1.25.x 另测。升级测试工具链时，在证据中注明新版本，不能只把版本字符串改成 latest。

性能首轮建议固定 1k/10k/50k 文件三档，并记录每档总字节量；单独提供大文件流式场景。在同一主机上分别测单请求及当前最大并发，至少记录 p50/p95、运行次数、冷热缓存条件和退化原因。此为实施时的测量计划，本文不提供未经运行的性能数字或 SLA。高成本基准在 P5 和相关性能变更时执行，常规 PR 保留必要功能门禁。

## 9. 替换旧断言

以下位置对应 [固定基线](https://github.com/fredgnr/gopls-mcp-proxy/commit/32cbd9fe11108f9f897ae1ef1cabbc94b10d1b51)：

| 旧位置 | 当前断言 | 必须替换为 |
|---|---|---|
| internal/workspace/tracker_test.go / TestFingerprintRules | go_content=false、atomic_go_save=false | 内容改变时 fingerprint 改变；同内容重写的 fingerprint 可不变，但事件 epoch 仍可使代次失效 |
| internal/proxy/supervisor_test.go / TestAtomicSaveAndGeneratorBurst | 内容原子保存后 generation=1 | 相应写入被检测后合并重建；有限生成批次仍去抖 |
| integration/real_gopls_test.go 的内容编辑分支 | 依赖 gopls roots watcher 看见新内容，generation 不变 | 必须有 proxy 独立触发的新后端和结果证据 |
| TestCrashLoadingFailureAndSchemaDrift | 业务 schema/description 摘要不同即 fatal | 拆成崩溃恢复、合法目录变化通知/快照比较、真正会话不兼容重连 |
| TestQueueCancellationLimitsAndPingDuringRebuild | 重建期间 proxy 本地 ping 永远成功 | 原生 ping 按控制请求处理，不伪造后端健康；有界等待另验收 |

保留原测试中依赖内容、漏事件、请求总期限、不重放、候选淘汰、进程回收等有效意图。迁移到通用请求与 raw transcript 后删除不再适用的工具专用断言，不维持互相冲突的两套“正确行为”。

## 10. CI 与结果记录

两类门禁分别输出：

1. **Proxy 契约门禁**：raw helper、内容监测、协议 profile、代次/取消/不重放、生命周期；Linux/macOS 默认运行，不下载 gopls 也能验证核心责任。
2. **固定 gopls 互通门禁**：真实依赖升级、内容编辑后重建、原生调用对照。缺少 gopls 的一般单元任务可以跳过这一类，但专门的互通任务不得静默跳过。

保留基线 CI 的 gofmt、go vet、go test -race、build、最低 Go、固定 Actions SHA、contents: read 和 persist-credentials: false。协议改造不需要 pull_request_target、release 权限或额外生产凭据。

结果报告至少包含下表字段：

| 字段 | 要求 |
|---|---|
| source_commit / tree | 对应实际被测代码 |
| test_id / revision / platform | 用例、具体协议修订与平台 |
| phase / assertion_scope | 阶段及实际执行的子断言；局部通过不等于完整 VAL 通过 |
| fixture_self_check | 无自刷新夹具自证结果 |
| command / exit_status | 实际命令和退出状态 |
| proxy / connection / boot / generation | 可追踪的进程与连接关联 |
| input / event / dispatch / terminal | 因果链与单调时间 |
| replay_count / resource_cleanup | 独立执行计数和资源回收事实 |
| limitations / status | pass、fail、pending、明确 skip 原因；不将缺证据写为通过 |

## 11. 需求覆盖

| 需求 | 主要验收组 |
|---|---|
| R01 独立刷新 | FIX、INPUT、GEN、GOPLS |
| R02 业务无感 | CLI、RAW、MCP-010 |
| R03 真实身份与能力 | MCP-002/005/006、RAW-003 |
| R04 同版本兼容 | MCP-001–010 |
| R05 串行发布 | GEN、LIFE |
| R06 请求与连接隔离 | ID、GEN-002/003、CANCEL |
| R07 业务不重放 | NOREPLAY、STATE-002/003/005 |
| R08 有界等待 | INPUT-009/010、CANCEL-004、LIFE |
| R09 检查点一致性与副作用 | INPUT-004/010、ERROR |
| R10 正常透传/恢复区分 | STATE、MCP |
| R11 明确重连 | MCP-003/009/010、STATE-006、LIFE-001 |
| R12 独立跨平台证明 | FIX、CI、GOPLS |

验收完成后，在对应实现提交的报告填写结果；本方案文件不预填任何运行通过结论。
