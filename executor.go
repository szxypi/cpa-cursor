package main

// Executor: one client chat becomes either
//
//   - a ChatService StreamUnifiedChatWithTools call (agentic turns with tool
//     calls/results; buffered protobuf frames parsed incrementally), or
//   - an AgentService Run over native h2 (plain-text turns, the duplex stream
//     Cursor's retired ChatService no longer accepts for them)
//
// both translated to OpenAI chat.completion chunks, exactly like 9router's
// CursorExecutor.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcExecutorRequest is what CPA sends for executor.* methods: the public
// ExecutorRequest plus the two ids the native ABI needs to talk back to the
// host (the client stream to emit into, and the callback scope that carries
// proxy policy and request-log capture).
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// preparedChat is everything needed to talk to Cursor for one call.
type preparedChat struct {
	identity    cursorIdentity
	parsed      *parsedChat
	model       string
	clientModel string
	// agentPath selects the AgentService transport (plain-text turns).
	agentPath bool
	// forceAgent presents the ChatService turn as agentic (Claude Code UA).
	forceAgent bool
	binding    [32]byte
	toolErrors map[string]bool
	stats      *agentTurnStats
}

func prepareChat(req rpcExecutorRequest) (*preparedChat, *statusError) {
	cred, err := parseCursorCredential(req.StorageJSON)
	if err != nil {
		return nil, newStatusError(http.StatusUnauthorized, "invalid_credential",
			"cursor credential is unusable; import a fresh token: "+err.Error())
	}
	body := executorBody(req)
	parsed, err := parseChatRequest(body)
	if err != nil {
		return nil, newStatusError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	model := strings.TrimPrefix(strings.TrimSpace(req.Model), providerKey+"/")
	if model == "" {
		return nil, newStatusError(http.StatusBadRequest, "invalid_request", "cursor request carries no model")
	}

	// Claude Code's user agent forces Cursor's agent mode on the ChatService
	// path too (9router's forceAgentMode), so an empty agentic flag still
	// presents as an agent turn to the backend.
	forceAgentMode := isClaudeCodeClient(req.Headers.Get("User-Agent"))

	return &preparedChat{
		identity:    cred.identity(),
		parsed:      parsed,
		model:       model,
		clientModel: strings.TrimSpace(req.Model),
		// ChatService 已以「客户端版本过旧」拒绝请求；外来工具历史在 parsed.Messages 中
		// 已渲染为文本，同样可以走 AgentService。
		agentPath:  true,
		binding:    agentRequestBinding(req.ExecutorRequest, model),
		toolErrors: originalToolErrors(req.OriginalRequest),
		stats:      &agentTurnStats{},
		forceAgent: forceAgentMode,
	}, nil
}

// isClaudeCodeClient mirrors 9router's UA sniffing for agent-mode forcing.
func isClaudeCodeClient(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	return strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code")
}

// executorBody prefers the payload CPA already translated into this executor's
// declared input format, falling back to the untouched client body.
func executorBody(req rpcExecutorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

// hostFramesChunks reports whether this plugin's chunks must be SSE-framed
// ("data: {...}") instead of bare JSON: the openai->claude stream translator
// drops chunks that are not already framed, and the request path in the host
// metadata is the only signal for which client protocol is downstream.
func hostFramesChunks(req rpcExecutorRequest) bool {
	return strings.HasPrefix(metadataString(req.Metadata, "request_path"), "/v1/messages")
}

func metadataString(metadata map[string]any, key string) string {
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// ==================== chunk emission ====================

// chunkEmitter turns parsed upstream events into OpenAI chunks, buffering
// composer-model thinking into visible content the way 9router does.
type chunkEmitter struct {
	model       string
	clientModel string
	composer    bool
	frameHost   bool
	emit        func([]byte) error

	id             string
	created        int64
	finish         string
	toolOrder      []string
	toolSeen       map[string]bool
	toolPending    map[string]*toolCallResult
	outputChars    int
	stats          *agentTurnStats
	thinking       strings.Builder
	visibleEmitted int
}

func newChunkEmitter(clientModel string, composer bool, frameHost bool, emit func([]byte) error) *chunkEmitter {
	return &chunkEmitter{
		model:       clientModel,
		clientModel: clientModel,
		composer:    composer,
		frameHost:   frameHost,
		emit:        emit,
		id:          fmt.Sprintf("chatcmpl-msg_%d", time.Now().UnixMilli()),
		created:     time.Now().Unix(),
		toolSeen:    map[string]bool{},
		toolPending: map[string]*toolCallResult{},
	}
}

// baseChunk assembles the shared envelope.
func (e *chunkEmitter) baseChunk(delta map[string]any) map[string]any {
	return map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.clientModel,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
}

func (e *chunkEmitter) sendChunk(chunk map[string]any) error {
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if e.frameHost {
		raw = append([]byte("data: "), raw...)
		raw = append(raw, '\n', '\n')
	}
	return e.emit(raw)
}

// textDelta forwards plain response text (and, for composer models, the part
// of the thinking buffer after </think>, emitted incrementally).
func (e *chunkEmitter) textDelta(delta string) error {
	if delta == "" {
		return nil
	}
	e.outputChars += len(delta)
	return e.sendChunk(e.baseChunk(map[string]any{"content": delta}))
}

// thinkingDelta buffers reasoning; composer models surface the post-</think>
// portion as visible content (9router's visibleComposerContentFromThinking).
func (e *chunkEmitter) thinkingDelta(delta string) error {
	if delta == "" {
		return nil
	}
	e.thinking.WriteString(delta)
	if !e.composer {
		// Reasoning is deliberately not forwarded to non-composer clients:
		// strict Anthropic clients discard unsigned thinking blocks.
		return nil
	}
	visible := visibleComposerContent(e.thinking.String())
	if len(visible) > e.visibleEmitted {
		next := visible[e.visibleEmitted:]
		e.visibleEmitted = len(visible)
		e.outputChars += len(next)
		return e.sendChunk(e.baseChunk(map[string]any{"content": next}))
	}
	return nil
}

// reasoningDelta 以 OpenAI reasoning_content 实时转发 AgentService 的思考流；宿主会把它
// 转成客户端格式（如 Anthropic thinking 块），让客户端在模型思考期间就能看到进度。
func (e *chunkEmitter) reasoningDelta(delta string) error {
	if delta == "" {
		return nil
	}
	e.thinking.WriteString(delta)
	return e.sendChunk(e.baseChunk(map[string]any{"reasoning_content": delta}))
}

// visibleComposerContent returns everything after the closing </think> tag,
// or everything when the model never opened one.
func visibleComposerContent(thinking string) string {
	idx := strings.LastIndex(thinking, "</think>")
	if idx < 0 {
		return thinking
	}
	return thinking[idx+len("</think>"):]
}

// toolCall buffers partial frames and emits once Cursor marks the call
// complete (isLast); flushPendingToolCalls covers streams that never do.
func (e *chunkEmitter) toolCall(call *toolCallResult) error {
	if call == nil {
		return nil
	}
	if done, ok := e.toolSeen[call.ID]; ok && done {
		return nil
	}
	e.toolSeen[call.ID] = call.IsLast
	if !call.IsLast {
		e.toolPending[call.ID] = call
		return nil
	}
	delete(e.toolPending, call.ID)
	e.outputChars += len(call.Name) + len(call.Arguments)
	e.toolOrder = append(e.toolOrder, call.ID)
	e.finish = "tool_calls"
	index := len(e.toolOrder) - 1
	return e.sendChunk(e.baseChunk(map[string]any{
		"tool_calls": []any{map[string]any{
			"index": index,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}},
	}))
}

// flushPendingToolCalls emits buffered partial calls at stream end.
func (e *chunkEmitter) flushPendingToolCalls() error {
	for _, call := range e.toolPending {
		if err := e.toolCall(call); err != nil {
			return err
		}
	}
	return nil
}

// sawToolCalls reports whether any tool call was emitted, so a turn with no
// text and no tools can be reported as an upstream failure.
func (e *chunkEmitter) sawToolCalls() bool {
	return len(e.toolSeen) > 0
}

// finishChunk closes the stream with the usage estimate.
func (e *chunkEmitter) finishChunk(promptChars int) error {
	finish := e.finish
	if finish == "" {
		finish = "stop"
	}
	prompt, completion := estimateTokensLen(promptChars), estimateTokensLen(e.outputChars)
	in, out, cacheRead, hasIn, hasOut := e.stats.usage()
	if hasIn {
		prompt = int(in)
	}
	if hasOut && int(out) > completion {
		completion = int(out)
	}
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if hasIn && cacheRead > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheRead}
	}
	chunk := e.baseChunk(map[string]any{})
	choices := chunk["choices"].([]any)
	choices[0].(map[string]any)["finish_reason"] = finish
	chunk["usage"] = usage
	if err := e.sendChunk(chunk); err != nil {
		return err
	}
	done := []byte("[DONE]")
	if e.frameHost {
		done = []byte("data: [DONE]\n\n")
	}
	return e.emit(done)
}

// estimateTokensLen is the chars/4 rule from translate.go, for pre-computed
// lengths.
func estimateTokensLen(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// ==================== stream handlers ====================

func handleExecutorExecuteStream(request []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelopeStatus("executor_error", "stream_id is required for executor.execute_stream", http.StatusInternalServerError), nil
	}

	prepared, statusErr := prepareChat(req)
	if statusErr != nil {
		return statusErr.envelope(), nil
	}

	if prepared.agentPath {
		return streamAgentTurn(prepared, req, streamID), nil
	}
	go func() {
		emitter := newChunkEmitter(prepared.clientModel, isComposerModel(prepared.model), hostFramesChunks(req), func(chunk []byte) error {
			return hostStreamEmit(streamID, chunk)
		})
		if err := runPrepared(prepared, req.HostCallbackID, emitter); err != nil {
			hostStreamClose(streamID, err.Error())
			return
		}
		if err := emitter.finishChunk(prepared.parsed.InputChars); err != nil {
			hostStreamClose(streamID, err.Error())
			return
		}
		hostStreamClose(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func handleExecutorExecute(request []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	prepared, statusErr := prepareChat(req)
	if statusErr != nil {
		return statusErr.envelope(), nil
	}

	aggregator := newCompletionAggregator(prepared.clientModel)
	emitter := newChunkEmitter(prepared.clientModel, isComposerModel(prepared.model), false, aggregator.consume)
	emitter.stats = prepared.stats
	if err := runPrepared(prepared, req.HostCallbackID, emitter); err != nil {
		return err.envelope(), nil
	}
	if err := emitter.finishChunk(prepared.parsed.InputChars); err != nil {
		return asStatusError(err, http.StatusBadGateway, "upstream_error").envelope(), nil
	}
	payload, err := aggregator.completion()
	if err != nil {
		return newStatusError(http.StatusBadGateway, "upstream_error", err.Error()).envelope(), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// runPrepared drives the upstream call and feeds the emitter; the returned
// error is already classified for the client.
func runPrepared(prepared *preparedChat, hostCallbackID string, emitter *chunkEmitter) *statusError {
	if prepared.agentPath {
		return runAgentTurn(prepared, emitter)
	}
	return runChatService(prepared, hostCallbackID, emitter)
}

// isComposerModel mirrors 9router: composer models stream their visible
// answer inside the thinking channel, fenced by </think>.
func isComposerModel(model string) bool {
	return strings.Contains(model, "composer")
}

// streamAgentTurn 在首个输出（文本或工具调用）到达前不向宿主开流：此前出现的错误仍以
// 带状态码的错误信封返回，宿主据此正确区分请求错误与凭据故障，不会误冷却；首个输出
// 之后实时转发，客户端不必等整轮生成完才看到内容。
func streamAgentTurn(prepared *preparedChat, req rpcExecutorRequest, streamID string) []byte {
	started := time.Now()
	var mu sync.Mutex
	var buffered [][]byte
	live := false
	firstAt := time.Duration(0)
	first := make(chan struct{})
	var firstOnce sync.Once
	emitter := newChunkEmitter(prepared.clientModel, isComposerModel(prepared.model), hostFramesChunks(req), func(chunk []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if !live {
			if firstAt == 0 {
				firstAt = time.Since(started)
			}
			buffered = append(buffered, append([]byte(nil), chunk...))
			firstOnce.Do(func() { close(first) })
			return nil
		}
		return hostStreamEmit(streamID, chunk)
	})
	emitter.stats = prepared.stats
	done := make(chan *statusError, 1)
	go func() {
		status := runPrepared(prepared, req.HostCallbackID, emitter)
		if status == nil {
			if err := emitter.finishChunk(prepared.parsed.InputChars); err != nil {
				status = asStatusError(err, 500, "stream_error")
			}
		}
		done <- status
	}()
	logTurn := func(status *statusError) {
		mode, rejects := prepared.stats.summary()
		outcome := "ok"
		if status != nil {
			outcome = fmt.Sprintf("%d %s", status.status, status.message)
		}
		mu.Lock()
		firstMS := firstAt.Milliseconds()
		mu.Unlock()
		hostLog("info", fmt.Sprintf("turn model=%s mode=%s first_ms=%d total_ms=%d finish=%s native_rejects=%s tokens=[%s] outcome=%s",
			prepared.model, mode, firstMS, time.Since(started).Milliseconds(), emitter.finish, rejects, prepared.stats.tokenSummary(), outcome))
	}
	discardTools := func() {
		for _, id := range emitter.toolOrder {
			pendingAgentTools.discard(id)
		}
	}
	headers := okEnvelopeMust(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})

	select {
	case status := <-done:
		logTurn(status)
		if status != nil {
			discardTools()
			return status.envelope()
		}
		go func() {
			for _, chunk := range buffered {
				if err := hostStreamEmit(streamID, chunk); err != nil {
					discardTools()
					hostStreamClose(streamID, err.Error())
					return
				}
			}
			hostStreamClose(streamID, "")
		}()
		return headers
	case <-first:
	}
	go func() {
		mu.Lock()
		var emitErr error
		for _, chunk := range buffered {
			if emitErr = hostStreamEmit(streamID, chunk); emitErr != nil {
				break
			}
		}
		buffered = nil
		live = emitErr == nil
		mu.Unlock()
		status := <-done
		logTurn(status)
		switch {
		case emitErr != nil:
			discardTools()
			hostStreamClose(streamID, emitErr.Error())
		case status != nil:
			discardTools()
			hostStreamClose(streamID, status.Error())
		default:
			hostStreamClose(streamID, "")
		}
	}()
	return headers
}

func okEnvelopeMust(payload map[string]any) []byte {
	envelope, _ := okEnvelope(payload)
	return envelope
}
