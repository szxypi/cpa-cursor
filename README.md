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
  - 带工具定义或工具结果的对话 → 同样走 AgentService（见下方 v0.3.14 工具桥接）。`api2.cursor.sh` 的 `ChatService/StreamUnifiedChatWithTools` 现已被 Cursor 以「客户端版本过旧」拒绝，v0.3.14 起不再使用（代码保留）。
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
- 流式请求按整轮缓冲后再下发（避免流中途出错被宿主当作凭据故障而冷却），客户端看不到逐字输出。
- 普通 system 指令不具备 Cursor 原生 system 的优先级（见 v0.3.13）。
- 未实现管理面板页（`management_api` 关闭）；凭证管理走 CPA 自带的「认证文件」页即可。
