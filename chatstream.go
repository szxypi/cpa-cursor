package main

// ChatService transport: host-callback streaming POST, with the ConnectRPC
// frame sequence parsed incrementally (frames become OpenAI chunks as soon as
// they are complete, instead of waiting for the whole body like 9router).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func contextBackgroundWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

// chatUpstream reads one host-streamed ChatService response.
type chatUpstream struct {
	streamID string
}

func openChatUpstream(callbackID, url string, headers map[string]string, body []byte) (*chatUpstream, *statusError) {
	resp, err := hostHTTPDoStream(callbackID, http.MethodPost, url, headers, body)
	if err != nil {
		return nil, asStatusError(err, http.StatusBadGateway, "upstream_error")
	}
	if resp.StatusCode != http.StatusOK {
		hostHTTPStreamClose(resp.StreamID)
		return nil, newStatusError(upstreamStatusToClient(resp.StatusCode), "upstream_error",
			fmt.Sprintf("cursor ChatService %d", resp.StatusCode))
	}
	return &chatUpstream{streamID: resp.StreamID}, nil
}

// upstreamStatusToClient keeps quota and auth signals distinguishable so CPA's
// cooldown and fallback machinery can act on them.
func upstreamStatusToClient(status int) int {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusUnauthorized
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

func (u *chatUpstream) close() {
	hostHTTPStreamClose(u.streamID)
}

// read blocks for the next payload; ok is false once the stream is finished.
func (u *chatUpstream) read() ([]byte, bool) {
	for {
		resp, err := hostHTTPStreamRead(u.streamID)
		if err != nil {
			return nil, false
		}
		if resp.Done {
			return nil, false
		}
		if len(resp.Payload) > 0 {
			return resp.Payload, true
		}
	}
}

// runChatService posts the protobuf body through the host callbacks and
// translates response frames into emitter events.
func runChatService(prepared *preparedChat, hostCallbackID string, emitter *chunkEmitter) *statusError {
	headers := buildCursorHeaders(prepared.identity)
	body := buildChatRequest(prepared.parsed.Messages, prepared.model, prepared.parsed.Tools, prepared.parsed.ReasoningEffort, prepared.forceAgent)

	resp, statusErr := openChatUpstream(hostCallbackID, cursorChatBase+cursorChatPath, headers, body)
	if statusErr != nil {
		return statusErr
	}
	defer resp.close()

	emitted := false
	decoder := newFrameDecoder()
	for {
		chunk, ok := resp.read()
		if !ok {
			break
		}
		textDeltas, thinkingDeltas, toolCalls, fatal := decoder.feed(chunk)
		if fatal != nil {
			// An upstream error only matters before anything was emitted;
			// afterwards the client already has partial content and the
			// stream simply ends.
			if !emitted {
				return classifyUpstreamError(fatal.Error())
			}
			break
		}
		for _, delta := range textDeltas {
			emitted = true
			if err := emitter.textDelta(delta); err != nil {
				return asStatusError(err, http.StatusBadGateway, "emit_failed")
			}
		}
		for _, delta := range thinkingDeltas {
			if err := emitter.thinkingDelta(delta); err != nil {
				return asStatusError(err, http.StatusBadGateway, "emit_failed")
			}
		}
		for _, call := range toolCalls {
			emitted = true
			if err := emitter.toolCall(call); err != nil {
				return asStatusError(err, http.StatusBadGateway, "emit_failed")
			}
		}
	}
	// Cursor marks tool calls complete with isLast; flush anything the stream
	// ended without marking.
	if err := emitter.flushPendingToolCalls(); err != nil {
		return asStatusError(err, http.StatusBadGateway, "emit_failed")
	}
	return nil
}

// classifyUpstreamError maps decoded error frames onto client statuses the
// way createErrorResponse does (resource_exhausted becomes rate-limit).
func classifyUpstreamError(message string) *statusError {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") {
		return newStatusError(http.StatusTooManyRequests, "rate_limit_error", message)
	}
	if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "no subscription") {
		return newStatusError(http.StatusUnauthorized, "invalid_credential", message)
	}
	return newStatusError(http.StatusBadRequest, "api_error", message)
}

// runAgentTurn drives the native-h2 AgentService run for plain-text turns.
func runAgentTurn(prepared *preparedChat, emitter *chunkEmitter) *statusError {
	if len(prepared.parsed.Tools) > 0 || hasAgentToolResult(prepared.parsed.RawMessages) {
		return runAgentToolBridge(prepared, emitter)
	}
	headers := buildCursorHeaders(prepared.identity)
	requestContext := newAgentContext(prepared.parsed.Messages)
	runFrame := buildAgentRunFrame(prepared.parsed.Messages, prepared.model, requestContext)

	ctx, cancel := contextBackgroundWithTimeout()
	defer cancel()
	client, _, err := openAgentStream(ctx, cursorAgentBase+cursorAgentRun, headers)
	if err != nil {
		return newStatusError(http.StatusBadGateway, "upstream_error",
			"cursor AgentService request failed: "+err.Error())
	}
	defer client.close()
	client.context = requestContext

	result := client.runTurn(runFrame, func(delta string) error {
		return emitter.textDelta(delta)
	}, traceAgentModel(prepared.model))
	recordAgentTrace(prepared.model, result)
	if result.Fatal != "" {
		return classifyAgentFailure(result.Fatal)
	}
	if strings.TrimSpace(result.Text) == "" && !emitter.sawToolCalls() {
		if result.TrailerCode != "" && result.TrailerCode != "ok" {
			return classifyAgentFailure("cursor AgentService " + result.TrailerCode)
		}
		return newStatusError(http.StatusBadGateway, "upstream_error", "cursor AgentService returned no content")
	}
	return nil
}

// classifyAgentFailure maps protocol-level agent failures onto client-facing
// statuses; auth trouble on the h2 path shows up as a transport error string.
func classifyAgentFailure(message string) *statusError {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "401"), strings.Contains(lower, "403"):
		return newStatusError(http.StatusUnauthorized, "invalid_credential", message)
	case strings.Contains(lower, "429"), strings.Contains(lower, "resource_exhausted"):
		return newStatusError(http.StatusTooManyRequests, "rate_limit_error", message)
	case strings.Contains(lower, "invalid_argument"), strings.Contains(lower, "invalid_request_error"):
		return newStatusError(http.StatusBadRequest, "invalid_request_error", "cursor AgentService rejected the request: "+message)
	case strings.Contains(lower, "eof"), strings.Contains(lower, "tls"), strings.Contains(lower, "connection reset"):
		return newStatusError(http.StatusBadGateway, "upstream_network_error", message)
	default:
		return newStatusError(http.StatusBadGateway, "upstream_error", message)
	}
}

// ==================== incremental ConnectRPC frame decoder ====================

// frameDecoder turns raw response bytes into decoded frame results.
type frameDecoder struct {
	pending   []byte
	toolCalls map[string]*toolCallResult
	order     []string
	failed    bool
}

func newFrameDecoder() *frameDecoder {
	return &frameDecoder{toolCalls: map[string]*toolCallResult{}}
}

// feed consumes one read chunk and returns the events it completed.
func (d *frameDecoder) feed(chunk []byte) (textDeltas, thinkingDeltas []string, toolCalls []*toolCallResult, fatal error) {
	if d.failed {
		return nil, nil, nil, nil
	}
	d.pending = append(d.pending, chunk...)
	for len(d.pending) >= 5 {
		flags := d.pending[0]
		length := int(uint32(d.pending[1])<<24 | uint32(d.pending[2])<<16 | uint32(d.pending[3])<<8 | uint32(d.pending[4]))
		if len(d.pending) < 5+length {
			break
		}
		payload := d.pending[5 : 5+length]
		d.pending = d.pending[5+length:]
		if flags&0x02 != 0 { // trailer
			continue
		}
		if flags&0x01 != 0 {
			decompressed, err := gunzip(payload)
			if err != nil {
				continue
			}
			payload = decompressed
		}
		result := extractChatResult(payload)
		if result.Error != "" {
			fatal = fmt.Errorf("cursor upstream error: %s", result.Error)
			d.failed = true
			return
		}
		if result.Text != "" {
			textDeltas = append(textDeltas, result.Text)
		}
		if result.Thinking != "" {
			thinkingDeltas = append(thinkingDeltas, result.Thinking)
		}
		if call := result.ToolCall; call != nil {
			// 9router emits every partial frame as an arguments delta; isLast
			// only marks the final one for this call id.
			d.toolCalls[call.ID] = call
			d.order = append(d.order, call.ID)
			toolCalls = append(toolCalls, call)
		}
	}
	return textDeltas, thinkingDeltas, toolCalls, nil
}

// pendingToolCalls returns calls the stream closed without marking isLast.
func (d *frameDecoder) pendingToolCalls() []*toolCallResult {
	var out []*toolCallResult
	for _, id := range d.order {
		if call, ok := d.toolCalls[id]; ok && !call.IsLast {
			out = append(out, call)
		}
	}
	return out
}
