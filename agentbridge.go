package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const agentToolIDPrefix = "call_cpa_"
const agentSegmentTimeout = 5 * time.Minute
const agentMaxSessions = 64

// agentToolHandoffMessage 作为 MCP 调用的即时结果回给 Cursor：调用交给客户端执行，
// 本次 run 随即结束，Cursor 才会报告本轮真实用量；真实结果随下一次请求发送。
const agentToolHandoffMessage = "Tool call handed off to the client for execution. End your turn immediately: do not write any further text, do not retry or repeat the call. The actual result will arrive in the next message."

type agentBridgeEvent struct {
	Text     string
	Thinking string
	Tool     *agentToolRequest
	ID       string
	Done     bool
	Err      *statusError
}
type agentBridgeSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	client    *agentClient
	events    chan agentBridgeEvent
	closeOnce sync.Once
	release   func()
}

func (s *agentBridgeSession) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.client.close()
		if s.release != nil {
			s.release()
		}
	})
}
func (s *agentBridgeSession) publish(e agentBridgeEvent) error {
	select {
	case s.events <- e:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

var agentBridgeSlots = make(chan struct{}, agentMaxSessions)

func originalToolErrors(body []byte) map[string]bool {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	out := make(map[string]bool)
	if json.Unmarshal(body, &request) != nil {
		return out
	}
	for _, m := range request.Messages {
		var blocks []struct {
			Type    string `json:"type"`
			ID      string `json:"tool_use_id"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				out[b.ID] = b.IsError
			}
		}
	}
	return out
}

func bridgeFault(message string) *statusError {
	return newStatusError(http.StatusBadRequest, "invalid_request_error", message)
}

// 不保存下游密钥明文；认证、模型和工具集合都参与续请求绑定。
func agentRequestBinding(req pluginapi.ExecutorRequest, model string) [32]byte {
	identity := []string{req.AuthID, model}
	for _, key := range []string{"Authorization", "X-Api-Key", "X-Session-ID", "X-Opencode-Session"} {
		identity = append(identity, req.Headers.Get(key))
	}
	for _, key := range []string{"api_key", "api_key_id", "client_id"} {
		if v, ok := req.Metadata[key]; ok {
			b, _ := json.Marshal(v)
			identity = append(identity, string(b))
		}
	}
	if req.AuthID == "" {
		sum := sha256.Sum256(req.StorageJSON)
		identity = append(identity, fmt.Sprintf("%x", sum))
	}
	encoded, _ := json.Marshal(identity)
	return sha256.Sum256(encoded)
}
func canonicalArguments(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}
func agentHistoryHash(messages []openAIMessage) [32]byte {
	type canonical struct {
		Role, Text, ToolID string
		Calls              []openAIToolCall
	}
	out := make([]canonical, 0, len(messages))
	for _, m := range messages {
		calls := append([]openAIToolCall(nil), m.ToolCalls...)
		for i := range calls {
			calls[i].Function.Arguments = canonicalArguments(calls[i].Function.Arguments)
		}
		out = append(out, canonical{m.Role, contentText(m.Content), m.ToolCallID, calls})
	}
	b, _ := json.Marshal(out)
	return sha256.Sum256(b)
}
func agentToolsHash(tools []cursorTool) [32]byte {
	copyTools := append([]cursorTool(nil), tools...)
	for i := range copyTools {
		copyTools[i].Arguments = canonicalArguments(copyTools[i].Arguments)
	}
	b, _ := json.Marshal(copyTools)
	return sha256.Sum256(b)
}

// agentTrailingStart 返回末尾连续用户消息之前的位置；客户端常在工具结果后附带
// 提醒类用户消息（如 Claude Code 的 system-reminder），它们属于下一轮输入。
func agentTrailingStart(ms []openAIMessage) int {
	i := len(ms)
	for i > 0 && ms[i-1].Role == "user" {
		i--
	}
	return i
}

func startAgentBridge(p *preparedChat, cp *agentCheckpoint, resumeText string) (*agentBridgeSession, *statusError) {
	catalog, err := newAgentToolCatalog(p.parsed.Tools)
	if err != nil {
		return nil, bridgeFault("invalid Cursor client tool definitions: " + err.Error())
	}
	select {
	case agentBridgeSlots <- struct{}{}:
	default:
		return nil, bridgeFault("Cursor tool session capacity reached; retry later")
	}
	release := func() { <-agentBridgeSlots }
	ctx, cancel := context.WithCancel(context.Background())
	headerTimer := time.AfterFunc(30*time.Second, cancel)
	defer headerTimer.Stop()
	client, _, err := openAgentStream(ctx, cursorAgentBase+cursorAgentRun, buildCursorHeaders(p.identity))
	if err != nil {
		cancel()
		release()
		return nil, newStatusError(502, "upstream_network_error", "Cursor AgentService connection failed")
	}
	state := newAgentContext(p.parsed.Messages)
	if cp != nil {
		cp.apply(state, resumeText)
	}
	state.tools = catalog
	client.context = state
	client.stats.Store(p.stats)
	s := &agentBridgeSession{ctx: ctx, cancel: cancel, client: client, release: release, events: make(chan agentBridgeEvent, 128)}
	client.onThinking = func(text string) error { return s.publish(agentBridgeEvent{Thinking: text}) }
	handedOff := make(map[string]bool)
	client.onTool = func(tool *agentToolRequest) error {
		// 模型偶尔会在占位结果后重复同一调用；只交付一次，但每次都要应答上游。
		key := tool.Name + "\x00" + canonicalArguments(tool.Arguments)
		if !handedOff[key] {
			handedOff[key] = true
			if err := s.publish(agentBridgeEvent{Tool: tool, ID: agentToolIDPrefix + strings.ReplaceAll(randomUUID(), "-", "")}); err != nil {
				return err
			}
		}
		return client.write(tool.handoffFrame())
	}
	frame := buildAgentRunFrame(p.parsed.Messages, p.model, state)
	go func() {
		result := client.runTurn(frame, func(text string) error { return s.publish(agentBridgeEvent{Text: text}) }, traceAgentModel(p.model))
		recordAgentTrace(p.model, result)
		var status *statusError
		if result.Fatal != "" {
			status = classifyAgentFailure(result.Fatal)
		}
		_ = s.publish(agentBridgeEvent{Done: true, Err: status})
	}()
	// 本轮结束时 runTurn 会半关闭写端，此后心跳写失败属正常现象，不能据此取消会话，
	// 否则会抢在 Done 事件之前把成功的回复报成连接中断；真实断连由读循环报告。
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := client.write(wrapConnectFrame(fieldBytes(7, nil))); err != nil {
					return
				}
			}
		}
	}()
	return s, nil
}

func emitAgentBridgeEvent(e agentBridgeEvent, emitter *chunkEmitter) error {
	if e.Text != "" {
		return emitter.textDelta(e.Text)
	}
	if e.Thinking != "" {
		return emitter.reasoningDelta(e.Thinking)
	}
	if e.Tool != nil {
		return emitter.toolCall(&toolCallResult{ID: e.ID, Name: e.Tool.Name, Arguments: e.Tool.Arguments, IsLast: true})
	}
	return nil
}

// consumeAgentBridge 把一次 run 的全部事件转发给客户端，直到 run 结束；工具调用已在
// 上游以占位结果应答，run 结束后保存 checkpoint，供带回工具结果的下一次请求续接。
func consumeAgentBridge(s *agentBridgeSession, p *preparedChat, emitter *chunkEmitter) ([]agentBridgeEvent, *statusError) {
	defer s.close()
	timer := time.NewTimer(agentSegmentTimeout)
	defer timer.Stop()
	var delivered []agentBridgeEvent
	var text strings.Builder
	var calls []openAIToolCall
	handedOff := false
	for {
		select {
		case <-s.ctx.Done():
			return delivered, newStatusError(502, "upstream_error", "Cursor connection closed")
		case <-timer.C:
			return delivered, newStatusError(504, "upstream_error", "Cursor response timed out")
		case e := <-s.events:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(agentSegmentTimeout)
			if e.Done {
				if e.Err != nil {
					return delivered, e.Err
				}
				if !agentDeliveredContent(delivered) {
					return delivered, newStatusError(502, "upstream_error", "Cursor returned no content")
				}
				agentCheckpoints.save(p, s.client, agentAssistantMessage(text.String(), calls))
				return delivered, nil
			}
			// 工具交接后模型收到的是占位结果，其后的文字多为「已提交、等待结果」之类的说明，
			// 不转发给客户端；checkpoint 的回复文本也只含交付给客户端的部分，保证下一轮能匹配。
			if handedOff && e.Tool == nil {
				continue
			}
			if e.Text != "" {
				text.WriteString(e.Text)
			}
			if e.Tool != nil {
				handedOff = true
				call := openAIToolCall{ID: e.ID, Type: "function"}
				call.Function.Name = e.Tool.Name
				call.Function.Arguments = e.Tool.Arguments
				calls = append(calls, call)
			}
			if err := emitAgentBridgeEvent(e, emitter); err != nil {
				return delivered, newStatusError(502, "stream_error", "Cursor client response failed")
			}
			delivered = append(delivered, e)
		}
	}
}

// agentDeliveredContent 判断是否交付过正文或工具调用；只有思考内容不算有效回复。
func agentDeliveredContent(events []agentBridgeEvent) bool {
	for _, e := range events {
		if e.Text != "" || e.Tool != nil {
			return true
		}
	}
	return false
}

func hasAgentToolResult(messages []openAIMessage) bool {
	for _, m := range messages {
		if m.Role == "tool" && strings.HasPrefix(m.ToolCallID, agentToolIDPrefix) {
			return true
		}
	}
	return false
}

// runAgentToolBridge 优先以 checkpoint 续接（只发送工具结果或新用户消息）；没有可用
// checkpoint 时以完整历史开启新 run，历史中的工具结果以文本形式提供。
func runAgentToolBridge(p *preparedChat, emitter *chunkEmitter) *statusError {
	cp, text, miss := agentCheckpoints.take(p)
	if cp != nil {
		p.stats.setMode("checkpoint" + miss)
		session, status := startAgentBridge(p, cp, text)
		if status == nil {
			var delivered []agentBridgeEvent
			delivered, status = consumeAgentBridge(session, p, emitter)
			if status == nil || len(delivered) > 0 {
				return status
			}
		}
		miss = "checkpoint_failed"
	}
	if hasAgentToolResult(p.parsed.RawMessages) || len(p.parsed.RawMessages) > 1 {
		p.stats.setMode("fresh-history")
	} else {
		p.stats.setMode("fresh")
	}
	p.stats.setMiss(miss)
	session, status := startAgentBridge(p, nil, "")
	if status != nil {
		return status
	}
	_, status = consumeAgentBridge(session, p, emitter)
	return status
}

// agentTurnStats 汇总一个 HTTP 轮次的上游行为，用于日志定位慢与失败；不含请求内容。
type agentTurnStats struct {
	mu      sync.Mutex
	mode    string
	miss    string
	rejects map[int]int
	mapped  map[int]int
	// outTokens 累加 token_delta，是本 HTTP 轮次实际生成的 token；ended 之后的字段来自
	// Cursor turn_ended，是整个上游 run 的累计值。
	checkpoint string
	outTokens  int64
	ended      bool
	inTokens   int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

func (s *agentTurnStats) mapNative(kind int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.mapped == nil {
		s.mapped = make(map[int]int)
	}
	s.mapped[kind]++
	s.mu.Unlock()
}

func (s *agentTurnStats) mappedSummary() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make([]string, 0, len(s.mapped))
	for kind, count := range s.mapped {
		kinds = append(kinds, fmt.Sprintf("%d:%d", kind, count))
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ",")
}

func (s *agentTurnStats) setCheckpoint(v string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.checkpoint = v
	s.mu.Unlock()
}

func (s *agentTurnStats) addOutputTokens(n int64) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	s.outTokens += n
	s.mu.Unlock()
}

func (s *agentTurnStats) setTurnEnded(in, out, cacheRead, cacheWrite, reasoning int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ended = true
	s.inTokens, s.cacheRead, s.cacheWrite, s.reasoning = in, cacheRead, cacheWrite, reasoning
	if s.outTokens == 0 {
		s.outTokens = out
	}
	s.mu.Unlock()
}

// usage 返回上游报告的用量；ok=false 表示没有收到任何 token 信息，调用方应退回估算。
func (s *agentTurnStats) usage() (in, out, cacheRead int64, hasIn, hasOut bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inTokens, s.outTokens, s.cacheRead, s.ended && s.inTokens > 0, s.outTokens > 0
}

func (s *agentTurnStats) tokenSummary() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		return fmt.Sprintf("out=%d", s.outTokens)
	}
	return fmt.Sprintf("in=%d out=%d cache_read=%d cache_write=%d reasoning=%d", s.inTokens, s.outTokens, s.cacheRead, s.cacheWrite, s.reasoning)
}

func (s *agentTurnStats) setMiss(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.miss = reason
	s.mu.Unlock()
}

func (s *agentTurnStats) setMode(mode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *agentTurnStats) reject(kind int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.rejects == nil {
		s.rejects = make(map[int]int)
	}
	s.rejects[kind]++
	s.mu.Unlock()
}

func (s *agentTurnStats) summary() (string, string) {
	if s == nil {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make([]string, 0, len(s.rejects))
	for kind, count := range s.rejects {
		kinds = append(kinds, fmt.Sprintf("%d:%d", kind, count))
	}
	sort.Strings(kinds)
	mode := s.mode
	if s.miss != "" && strings.HasPrefix(mode, "fresh") {
		mode += "(" + s.miss + ")"
	}
	if s.checkpoint != "" {
		mode += " cp=" + s.checkpoint
	}
	return mode, strings.Join(kinds, ",")
}
