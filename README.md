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
  - 纯文本对话（消息全是纯文本，无 tool_calls/tool 结果）→ `agent.api5.cursor.sh` 的 `AgentService/Run`。该域名只收 HTTP/2 且是**双向流**（服务端中途发 `exec_request` 要 IDE 上下文，必须回写空 RequestContext），宿主的 http 回调表达不了写端，所以这条路径由插件内 `net/http` 直连（ALPN 协商 h2）；它因此不进 request-log、不走宿主代理策略。
  - 带工具的 agentic 对话 → `api2.cursor.sh` 的 `ChatService/StreamUnifiedChatWithTools`，走宿主 http 回调（request-log、代理策略都在）。Cursor 已对「带 schema 的纯文本轮」拒绝 ChatService，但真实工具对话仍走它（9router 同样如此）。
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

## 已知限制

- token 失效只能重新导入（无服务端刷新）。
- AgentService 直连路径绕过宿主代理与 request-log；ChatService 主路径不受影响。
- 未实现管理面板页（`management_api` 关闭）；凭证管理走 CPA 自带的「认证文件」页即可。
