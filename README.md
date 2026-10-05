# cpa-cursor

把 Cursor 订阅接入 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的插件：以官方 C ABI 注册一个名为 `cursor` 的提供方，用从 Cursor IDE 导出的 token 直连 Cursor 官方后端。

反代逻辑移植自 [9router](https://github.com/decolua/9router) 的 cursor 执行器：ConnectRPC protobuf 编解码、`x-cursor-checksum`（Jyh cipher）、client-key/session-id 派生、ChatService + AgentService 双路径与工具调用（MCP 形态）协议均与其对齐。

## 它做了什么

| CPA 能力 | 用途 |
| --- | --- |
| `auth_provider` | 解析/导入凭证。Cursor 没有服务端可跑的 OAuth（token 在 IDE 本地 `state.vscdb` 里），所以只做导入：CLI 一条命令，或手放 JSON 认证文件 |
| `model_provider` | 按账号调 `GetUsableModels`（agent.api5 上 h2 unary）拉取该账号真实可用模型，失败回退静态目录（9router registry 同款 14 个） |
| `executor` | 接收 CPA 翻译好的 OpenAI chat-completions 请求，编码成 protobuf 后发给 Cursor，把响应帧还原成标准 OpenAI SSE（流式增量、工具调用、composer 模型 `</think>` 剥离） |
| `command_line_plugin` | `cli-proxy-api --cursor-login <token>` |

## 协议要点（与 9router 对齐）

- **两条上游路径**：
  - 纯文本对话（消息全是纯文本，无 tool_calls/tool 结果）→ `agent.api5.cursor.sh` 的 `AgentService/Run`。该域名只收 HTTP/2 且是**双向流**（服务端通过 `exec_request` 获取 `RequestContext.rules`，通过 KV Get/Set 获取 SHA-256 历史消息 blob；必须完整回应两类握手），宿主的 http 回调表达不了写端，所以这条路径由插件内 `net/http` 直连（ALPN 协商 h2）；它因此不进 request-log、不走宿主代理策略。
  - 带工具定义或工具结果的对话 → 同样走 AgentService（见下方 v0.3.15–v0.3.28 工具交接）。`api2.cursor.sh` 的 `ChatService/StreamUnifiedChatWithTools` 现已被 Cursor 以「客户端版本过旧」拒绝，v0.3.14 起不再使用（代码保留）。
- **工具语义**：assistant 历史 tool_calls 不重发；tool 结果转成 user 消息里的 `<tool_result>` XML 文本块（避免 protobuf `tool_results` 的 schema 漂移死循环）；工具定义以 MCP 形态（`mcp_custom_<name>`）下发；响应侧 tool call 从 MCP 参数恢复名称与参数。
- **thinking**：reasoning 只对 composer 模型以「`</think>` 后内容」作为可见文本输出，其余模型不上报（无签名的 thinking 块会被 Claude Code 拒收）。
- **用量**：Cursor 协议不回 usage，按 9router 同款 chars/4 估算填 `usage`。

## 安装

```bash
./scripts/build.sh          # 产出 dist/cpa-cursor-v<版本>.so
sudo ops/merge-config.py    # 往 config.yaml 的 plugins.configs 里加一段
sudo ops/deploy             # 安装 .so 并重启 CPA
```

配置（`config.yaml` → `plugins.configs.cpa-cursor`）：

```yaml
    cpa-cursor:
      enabled: true
```

## 导入凭证

从一台登录过 Cursor 的机器上取 accessToken（Linux 在
`~/.config/Cursor/User/globalStorage/state.vscdb`，键
`cursorAuth/accessToken`，值 JSON 里的 `accessToken`；machineId 是同库键
`storage.serviceMachineId`）：

```bash
sqlite3 state.vscdb "SELECT value FROM itemTable WHERE key='cursorAuth/accessToken'"
sqlite3 state.vscdb "SELECT value FROM itemTable WHERE key='storage.serviceMachineId'"
```

```bash
cli-proxy-api --cursor-login '<accessToken>' \
  [--cursor-machine-id '<machineId>'] [--cursor-email '<email>']
```

machineId 省略时由 token 派生（`sha256(token+"machineId")`，与 9router 一致）。也可以手放 JSON：

```json
{"type": "cursor", "access_token": "...", "machine_id": "...", "email": "..."}
```

到 CPA 的 `auths/` 目录，`auth.parse` 会认领。凭证格式与 9router 的 cursor 凭据兼容（`access_token` / `accessToken` 两种键名都认）。

导入后模型即出现在 `/v1/models`（账号有权限的为准），用法与其它提供方一致：`model: cursor/claude-4.5-sonnet` 之类。

## v0.3.34 思考档位族模型

- **问题**：AgentService 的 run 请求只带模型 ID，不带思考档位。Cursor 为每个档位提供单独的 ID（如 `grok-4.7-low-fast` … `grok-4.7-xhigh-fast`）。插件以前只在已停用的 ChatService 路径里映射 medium/high，所以客户端发的 `reasoning_effort` 全部被忽略。
- **族模型**：插件从账号目录里找出只有档位不同的一组 ID，再注册一个族 ID，格式为 `cursor-<base>[-fast]`，例如 `cursor-grok-4.7-fast`、`cursor-claude-opus-5-5`。请求族 ID 时，插件按 `reasoning_effort` 选对应的变体。没有该档位时先向上取，再向下取。没有档位或档位无法识别时用 medium。族 ID 带 `cursor-` 前缀，是为了不和 xai 的同名模型合成一个池。
- 族模型在 `/v1/models` 里带 `thinking.levels`，列出可选的档位。面板的模型列表也列出族模型，要勾选后才会注册。
- 原有的变体 ID 行为不变，固定使用 ID 里的档位。
- 日志 `turn model=` 记录实际发给 Cursor 的变体 ID，响应里的 `model` 仍是客户端请求的族 ID。

## v0.3.33 checkpoint 可重复续接与按调用 ID 匹配

- **可重复续接**：checkpoint 续接后不再删除（内存与磁盘均是），只随 30 分钟过期或容量淘汰，同一会话最多保留 4 个。实测 Cursor 允许同一 checkpoint 多次续接，各分支互不可见。此前 Claude Code 的旁路请求（如 away summary）会先用掉 checkpoint，真实的下一条消息只能以完整历史重开（未命中原因 `prefix_len`）。
- **按调用 ID 匹配**：严格哈希未命中时，先按上一轮交给客户端的 `call_cpa_` 工具调用 ID 查找（日志 `mode=checkpoint-calls`），再走宽松匹配；宽松匹配不再比较工具入参。PreToolUse hook 改写 Bash 命令、上下文分页替换旧 tool_use 大入参后仍能续接（此前未命中原因为 `reply`、`prefix@i/n:tool`）。
- 续接时丢弃为本次历史新建的 blob，前后 checkpoint 共用 blob 数据，重复续接不会让内存随轮数翻倍。
- 已知取舍：工具交接后上游仍会对占位结果多跑一步（约 7s）才结束 run。保持 run 挂起、跨请求写回真实结果可以省掉这一步（实测持续心跳时 Cursor 可等至少 5 分钟），但工具轮响应就拿不到 `turn_ended`，无法上报真实的输入与缓存用量，因此保持现状。

## v0.3.29–v0.3.32 原生工具转交、宽松续接与 checkpoint 落盘

- **原生工具转交客户端**：Cursor 自带的 Shell（exec 2/14）、Grep（5）、Write（3，二进制除外）在调用方声明了 schema 兼容的同类工具时，转成 `Bash`/`Grep`/`Write`（或小写同名）的 `tool_calls` 交客户端执行，上游收到原生结果类型的「已转交」占位后结束本轮。Shell 的工作目录以 `cd '<dir>' &&` 前缀保留；只声明了 Bash 时，Grep 还原为等价的 `rg` 命令交给 Bash。参数无法一一对应时仍返回权限拒绝，并记录 `native reject kind= candidates=` 便于排查。
- **宽松续接**：上游中间件（如上下文分页、输出压缩）改写旧的 user/tool/system 文本会让严格哈希失配。现在消息条数、角色、工具调用 ID 一致且 assistant 消息与最终回复完全相同时，仍以 checkpoint 续接（日志 `mode=checkpoint-relaxed`）；checkpoint 保存的是改写前的完整内容。
- **交接后不再转发说明文字**：同一 run 中首个工具调用之后的文字和思考（多为「已提交、等待结果」）不再下发给客户端，也不计入 checkpoint 的回复匹配。
- **checkpoint 落盘**：保存到服务目录下的 `cpa-cursor/checkpoints/`（目录 0700、文件 0600，含会话内容），启动时载入未过期项，使用后删除，超过 30 分钟清理；CPA 重启或部署后可继续续接。
- 日志新增 `native_mapped=kind:count`；未命中原因 `prefix@i/n:role:lenA>B[:reminder]` 给出被改写消息的前后长度；非流式请求同样输出 `turn` 日志。

## v0.3.15–v0.3.28 速度、用量与工具交接

- **工具交接改为逐次结束上游 run**（取代 v0.3.14 的挂起流续接）：Cursor 请求 MCP 工具时，插件立即以占位结果应答（告知模型工具已交客户端执行、结束本轮），本次 run 正常结束并下发 `conversation_checkpoint_update`。客户端带回工具结果后，插件以 checkpoint 为会话状态开启新 run，只把工具结果（`<tool_result name id [is_error]>`）和其后的用户消息作为新输入，服务端不必重新处理整段历史。模型一次发起的多个工具调用会一并返回。
- **checkpoint 复用**：以纯文本或工具调用结束的每一轮都会保存 checkpoint（含服务端 KV 写入的 blob 与 conversation_id），键为调用方身份 + 该轮请求历史哈希 + 本轮回复（文本忽略空白，含 tool_calls），30 分钟有效、仅用一次。历史被改写、工具集变化或回复不符时不复用，以完整历史重开；checkpoint run 在产生输出前失败也会自动回退。
- **思考流**：AgentService 的 `thinking_delta` 以 `reasoning_content` 实时转发（CPA 转为 Anthropic thinking 块），模型思考期间客户端即可看到进度。流式响应在首个输出到达前缓冲（此前的错误仍以状态码返回），之后实时下发。
- **真实用量**：输出 token 取 `token_delta` 累加；输入与缓存取 `turn_ended`。由于 `turn_ended.input_tokens` 是 run 内各步累计值，上报的 `prompt_tokens` 不超过本次请求的估算大小，`cached_tokens` 按上游真实命中比例折算，避免 Claude Code 误判上下文已满而反复自动压缩。
- 其他：共享 h2 连接；无工具请求中模型发起的原生 IDE 操作返回拒绝而非 400；未知的原生 exec 字段以 throw 应答；非流式聚合保留 `reasoning_content` 与嵌套 usage。
- 日志：每轮输出 `turn binding= model= mode= first_ms= total_ms= finish= native_rejects= tokens=[...] outcome=`；`mode` 为 `checkpoint` / `plain-checkpoint` / `fresh` / `fresh-history(原因)`，未命中原因如 `prefix@i/n:role`、`prefix_len:a/b`、`reply`、`tool_results`、`no_checkpoint(binding)`。
- 实测：`TestAgentLiveToolRoundTrip` 要求第二轮起均以 checkpoint 续接；`TestAgentLiveToolFreshFallback` 每轮丢弃 checkpoint 验证完整历史回退。

## v0.3.14 AgentService 工具桥接

- 调用方声明的工具以 MCP 定义下发给 Cursor；Cursor 请求 MCP 工具时，插件把它转成标准 `tool_calls` 交给客户端执行，客户端下一次请求带回 `tool` 结果后写回同一条上游 run 继续生成。插件本身不执行任何命令或文件操作。
- Cursor 原生 IDE 工具：Read 在调用方声明了 schema 兼容的 `Read`/`read_file` 时转交客户端执行，其余（Shell、Write、Delete、Grep、Ls、Diagnostics）返回权限拒绝，让模型改用声明的工具。
- `provider_identifier` 由 Cursor 后端回填（实测为 `pi-agent`），不作为授权依据；是否可执行只看工具名是否在本次请求的工具列表中。
- 应答 `interaction_query`：托管的 WebSearch/WebFetch 批准（在 Cursor 服务端执行），提问、切换模式、建计划、MCP 授权、生成图片、PR 管理一律拒绝；VM/环境/SCM 类询问无法表达拒绝，直接以 400 结束本轮，不会无限挂起。
- 续接丢失（超过 10 分钟、CPA 重启、其他插件改写了历史、跨凭据重试等）时不再报 400，而是以完整历史重开一轮，工具结果以 `<tool_result>` 文本进入上下文。其他身份拿到相同 tool id 不影响原会话。
- 修复轮次结束时心跳写失败触发取消、把成功回复误报为「连接已关闭」的竞态；上游等待超时改为 5 分钟空闲超时。
- 实测：`TestAgentLiveToolRoundTrip`（同一 run 续接）与 `TestAgentLiveToolFreshFallback`（每轮丢弃会话后重开）均需显式提供 `CURSOR_PROBE_CREDENTIAL`，在隔离临时目录完成 Read → `stamp_result` → 用两个真实结果作答的闭环。

## v0.3.13 AgentService 兼容修复

- 不再将普通 system 写入团队专用的 `custom_system_prompt`（field 8）。当前后端会返回 `invalid_argument: unknown option '--system-prompt'`；改为通过 `RequestContext.rules` 传递原文。后端在多轮情况下仍可能忽略 system/rules，因此另将原文以明确来源标记的 user 历史 blob 保留；这是内容兼容副本，不是原生 system 权限等价物。
- 历史消息通过 `ConversationStateStructure.root_prompt_messages_json` 的 SHA-256 blob 引用和 KV Get/Set 握手传递，避免仅发送旧 history 字段时静默丢失上下文。
- finish 后半关闭请求并读完 RPC trailer；`invalid_argument` 明确返回请求级 400，而非空回复 502。下游写失败和取消会关闭上游 pipe。
- 合成实测覆盖系统背景信息、多轮事实回忆；不等于可以用普通应用规则覆盖 Cursor 自身系统指令。严格要求固定 `SYSTEM_OK` 来替代算术回答的探测仍未遵循，此项没有冒充通过。
- 单测：`go test -count=1 ./...` 和 `go test -race -count=1 ./...`。`agent_live_test.go` 仅在显式提供 `CURSOR_PROBE_CREDENTIAL` 时外发合成请求，默认跳过，不使用真实对话。

## 已知限制

- token 失效只能重新导入（无服务端刷新）。
- AgentService 直连路径绕过宿主代理与 request-log，出站代理只认进程环境变量（`HTTPS_PROXY`，本机由 systemd drop-in `10-proxy.conf` 提供）。
- 模型收到工具占位结果后仍可能在上游补一句说明（不再转发给客户端），会多消耗少量输出 token。
- 历史被截断、压缩为更少条数（如 `/compact`）或 assistant 消息被改写时无法续接，以完整历史重开。
- Cursor 原生 Delete、Ls、Diagnostics 等仍返回权限拒绝，由模型改用声明的工具。
- 普通 system 指令不具备 Cursor 原生 system 的优先级（见 v0.3.13）。
- 未实现管理面板页（`management_api` 关闭）；凭证管理走 CPA 自带的「认证文件」页即可。
