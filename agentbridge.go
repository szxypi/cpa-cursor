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
const agentPendingTTL = 10 * time.Minute
const agentSegmentTimeout = 5 * time.Minute
const agentMaxPending = 64

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

type agentPendingCall struct {
	id            string
	session       *agentBridgeSession
	tool          *agentToolRequest
	binding       [32]byte
	prefix        [32]byte
	prefixLen     int
	tools         [32]byte
	assistantText string
	state         string
	expiry        time.Time
	timer         *time.Timer
	resultHash    [32]byte
	replay        []agentBridgeEvent
	err           *statusError
}
type agentBridgeStore struct {
	mu    sync.Mutex
	calls map[string]*agentPendingCall
	now   func() time.Time
	limit int
	ttl   time.Duration
}

var agentBridgeSlots = make(chan struct{}, agentMaxPending)

var pendingAgentTools = agentBridgeStore{calls: make(map[string]*agentPendingCall), now: time.Now, limit: agentMaxPending, ttl: agentPendingTTL}

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

func (b *agentBridgeStore) park(id string, s *agentBridgeSession, tool *agentToolRequest, p *preparedChat, text string) *statusError {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	active := 0
	for key, call := range b.calls {
		if !now.Before(call.expiry) {
			if call.timer != nil {
				call.timer.Stop()
			}
			if call.state != "done" {
				call.session.close()
			}
			delete(b.calls, key)
			continue
		}
		if call.state != "done" {
			active++
		}
	}
	if active >= b.limit {
		return bridgeFault("Cursor tool continuation capacity reached; no tool has been executed")
	}
	if len(b.calls) >= b.limit*4 {
		for key, call := range b.calls {
			if call.state == "done" {
				delete(b.calls, key)
				break
			}
		}
	}
	call := &agentPendingCall{id: id, session: s, tool: tool, binding: p.binding, prefix: agentHistoryHash(p.parsed.RawMessages), prefixLen: len(p.parsed.RawMessages), tools: agentToolsHash(p.parsed.Tools), assistantText: text, state: "pending", expiry: now.Add(b.ttl)}
	b.calls[id] = call
	call.timer = time.AfterFunc(b.ttl, func() {
		b.mu.Lock()
		current := b.calls[id]
		if current == call && current.state == "pending" {
			delete(b.calls, id)
		} else {
			current = nil
		}
		b.mu.Unlock()
		if current != nil {
			current.session.close()
		}
	})
	return nil
}

// errAgentContinuationLost 表示没有可续接的上游会话（过期、重启、历史被改写等）；
// 调用方应以完整历史重开一轮，而不是让客户端报错。
var errAgentContinuationLost = bridgeFault("Cursor tool continuation is unavailable")

func (b *agentBridgeStore) claim(id string, p *preparedChat) (*agentPendingCall, bool, *statusError) {
	b.mu.Lock()
	call, stale, status := b.claimLocked(id, p)
	b.mu.Unlock()
	if stale != nil {
		stale.session.close()
	}
	if status != nil {
		return nil, false, status
	}
	return call, call.state == "done", nil
}

// claimLocked 在持锁状态下校验续接请求；返回的 stale 需在释放锁后关闭其上游会话。
func (b *agentBridgeStore) claimLocked(id string, p *preparedChat) (call, stale *agentPendingCall, status *statusError) {
	call = b.calls[id]
	if call == nil {
		p.stats.setMiss("no_pending")
		return nil, nil, errAgentContinuationLost
	}
	if call.binding != p.binding {
		p.stats.setMiss("binding")
		return nil, nil, errAgentContinuationLost
	}
	evict := func() *agentPendingCall {
		delete(b.calls, id)
		if call.timer != nil {
			call.timer.Stop()
		}
		if call.state == "done" {
			return nil
		}
		return call
	}
	if !b.now().Before(call.expiry) {
		p.stats.setMiss("expired")
		return nil, evict(), errAgentContinuationLost
	}
	// 同一调用方改写了历史（其他插件压缩工具输出、客户端重排消息等）时旧会话已无法对应，
	// 释放它并由调用方重开；当前按一个工具一轮交付，上游并发调用在队列中依次交付。
	ms := p.parsed.RawMessages
	miss := ""
	switch {
	case call.tools != agentToolsHash(p.parsed.Tools):
		miss = "tools"
	case len(ms) < call.prefixLen+2 || agentTrailingStart(ms) != call.prefixLen+2:
		miss = fmt.Sprintf("len:%d/%d", len(ms), call.prefixLen+2)
	case agentHistoryHash(ms[:call.prefixLen]) != call.prefix:
		miss = "prefix"
	}
	matches := miss == ""
	var result openAIMessage
	if matches {
		assistant := ms[call.prefixLen]
		result = ms[call.prefixLen+1]
		matches = assistant.Role == "assistant" && len(assistant.ToolCalls) == 1 && result.Role == "tool" && result.ToolCallID == id
		if matches {
			tc := assistant.ToolCalls[0]
			matches = tc.ID == id && tc.Function.Name == call.tool.Name && canonicalArguments(tc.Function.Arguments) == canonicalArguments(call.tool.Arguments) && contentText(assistant.Content) == call.assistantText
		}
		if !matches {
			miss = "assistant"
		}
	}
	if !matches {
		p.stats.setMiss(miss)
		if call.state == "pending" {
			return nil, evict(), errAgentContinuationLost
		}
		return nil, nil, errAgentContinuationLost
	}
	resultDigest := sha256.Sum256([]byte(fmt.Sprintf("%t\n%s", p.toolErrors[id], agentResumeText(ms, call.prefixLen+1))))
	if call.state != "pending" && call.resultHash != resultDigest {
		return nil, nil, bridgeFault("Cursor tool result cannot be changed after submission")
	}
	if call.state == "done" {
		return call, nil, nil
	}
	if call.state != "pending" {
		return nil, nil, bridgeFault("Cursor tool result is already being processed; do not execute it again")
	}
	call.state = "running"
	call.resultHash = resultDigest
	call.timer.Stop()
	call.expiry = b.now().Add(agentSegmentTimeout)
	return call, nil, nil
}
func (b *agentBridgeStore) complete(call *agentPendingCall, events []agentBridgeEvent, err *statusError) {
	b.mu.Lock()
	call.state = "done"
	call.replay = append([]agentBridgeEvent(nil), events...)
	call.err = err
	call.expiry = b.now().Add(b.ttl)
	call.timer = time.AfterFunc(b.ttl, func() {
		b.mu.Lock()
		if b.calls[call.id] == call && call.state == "done" {
			delete(b.calls, call.id)
		}
		b.mu.Unlock()
	})
	b.mu.Unlock()
}
func (b *agentBridgeStore) discard(id string) {
	b.mu.Lock()
	call := b.calls[id]
	delete(b.calls, id)
	if call != nil && call.timer != nil {
		call.timer.Stop()
	}
	shouldClose := call != nil && call.state != "done"
	b.mu.Unlock()
	// 已完成的调用可能仍被同 session 的后续工具复用，不能连带关闭。
	if shouldClose {
		call.session.close()
	}
}
func (b *agentBridgeStore) shutdown() {
	b.mu.Lock()
	calls := b.calls
	b.calls = make(map[string]*agentPendingCall)
	b.mu.Unlock()
	for _, call := range calls {
		if call.timer != nil {
			call.timer.Stop()
		}
		call.session.close()
	}
}

func startAgentBridge(p *preparedChat) (*agentBridgeSession, *statusError) {
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
	state.tools = catalog
	client.context = state
	client.stats.Store(p.stats)
	s := &agentBridgeSession{ctx: ctx, cancel: cancel, client: client, release: release, events: make(chan agentBridgeEvent, 128)}
	client.onThinking = func(text string) error { return s.publish(agentBridgeEvent{Thinking: text}) }
	client.onTool = func(tool *agentToolRequest) error {
		return s.publish(agentBridgeEvent{Tool: tool, ID: agentToolIDPrefix + strings.ReplaceAll(randomUUID(), "-", "")})
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
	// 读循环始终继续处理 KV 和其他 exec；心跳只保活，不代替客户端权限批准。
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
func consumeAgentBridge(s *agentBridgeSession, p *preparedChat, emitter *chunkEmitter) ([]agentBridgeEvent, *statusError) {
	timer := time.NewTimer(agentSegmentTimeout)
	defer timer.Stop()
	var delivered []agentBridgeEvent
	var text strings.Builder
	for {
		select {
		case <-s.ctx.Done():
			s.close()
			return delivered, newStatusError(502, "upstream_error", "Cursor continuation connection closed")
		case <-timer.C:
			s.close()
			return delivered, newStatusError(504, "upstream_error", "Cursor response timed out")
		case e := <-s.events:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(agentSegmentTimeout)
			if e.Done {
				s.close()
				if e.Err != nil {
					return delivered, e.Err
				}
				if !agentDeliveredContent(delivered) {
					return delivered, newStatusError(502, "upstream_error", "Cursor returned no content")
				}
				return delivered, nil
			}
			if e.Text != "" {
				text.WriteString(e.Text)
			}
			if e.Tool != nil {
				if err := pendingAgentTools.park(e.ID, s, e.Tool, p, text.String()); err != nil {
					s.close()
					return delivered, err
				}
			}
			if err := emitAgentBridgeEvent(e, emitter); err != nil {
				if e.Tool != nil {
					pendingAgentTools.discard(e.ID)
				} else {
					s.close()
				}
				return delivered, newStatusError(502, "stream_error", "Cursor client response failed")
			}
			delivered = append(delivered, e)
			if e.Tool != nil {
				return delivered, nil
			}
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

// agentTrailingStart 返回末尾连续用户消息之前的位置；客户端常在工具结果后附带
// 提醒类用户消息（如 Claude Code 的 system-reminder），它们不应阻断续接。
func agentTrailingStart(ms []openAIMessage) int {
	i := len(ms)
	for i > 0 && ms[i-1].Role == "user" {
		i--
	}
	return i
}

// agentResumeText 是续接时回传给上游的工具结果；工具结果之后的用户消息附在末尾，
// 因为已在运行中的上游会话只能接收这一条工具结果。
func agentResumeText(ms []openAIMessage, resultIndex int) string {
	text := contentText(ms[resultIndex].Content)
	for _, m := range ms[resultIndex+1:] {
		if extra := strings.TrimSpace(contentText(m.Content)); extra != "" {
			text += "\n\n<user_message>\n" + extra + "\n</user_message>"
		}
	}
	return text
}

func runAgentToolBridge(p *preparedChat, emitter *chunkEmitter) *statusError {
	ms := p.parsed.RawMessages
	last := agentTrailingStart(ms) - 1
	if last >= 0 && ms[last].Role == "tool" && strings.HasPrefix(ms[last].ToolCallID, agentToolIDPrefix) {
		result := ms[last]
		id := result.ToolCallID
		call, replay, status := pendingAgentTools.claim(id, p)
		if status == errAgentContinuationLost {
			// 工具结果已作为 <tool_result> 历史进入 parsed.Messages，重开一轮即可继续。
			return runFreshAgentBridge(p, emitter)
		}
		if status != nil {
			return status
		}
		if replay {
			p.stats.setMode("replay")
			for _, e := range call.replay {
				if err := emitAgentBridgeEvent(e, emitter); err != nil {
					return newStatusError(502, "stream_error", "tool replay delivery failed")
				}
			}
			return call.err
		}
		p.stats.setMode("resume")
		call.session.client.stats.Store(p.stats)
		// OpenAI tool 结果是实际客户端输出；不在网关执行，也不伪造输出。
		if err := call.session.client.write(call.tool.resultFrame(agentResumeText(ms, last), p.toolErrors[id])); err != nil {
			call.session.close()
			status = bridgeFault("Cursor tool continuation disconnected; result was not replayed")
			pendingAgentTools.complete(call, nil, status)
			return status
		}
		events, status := consumeAgentBridge(call.session, p, emitter)
		pendingAgentTools.complete(call, events, status)
		return status
	}
	if last >= 0 {
		if ms[last].Role != "tool" {
			p.stats.setMiss("last:" + ms[last].Role)
		} else {
			p.stats.setMiss("foreign_id")
		}
	}
	return runFreshAgentBridge(p, emitter)
}

// runFreshAgentBridge 以完整历史开启新的上游 run；历史中的工具调用结果只作为上下文。
func runFreshAgentBridge(p *preparedChat, emitter *chunkEmitter) *statusError {
	if hasAgentToolResult(p.parsed.RawMessages) {
		p.stats.setMode("fresh-history")
	} else {
		p.stats.setMode("fresh")
	}
	session, status := startAgentBridge(p)
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
	// outTokens 累加 token_delta，是本 HTTP 轮次实际生成的 token；ended 之后的字段来自
	// Cursor turn_ended，是整个上游 run 的累计值。
	outTokens  int64
	ended      bool
	inTokens   int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
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
	return mode, strings.Join(kinds, ",")
}
