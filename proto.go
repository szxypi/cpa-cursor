package main

// Cursor ConnectRPC protobuf wire codec, ported from 9router's
// open-sse/utils/cursorProtobuf.js (schema v1.1.3). Field numbers below are
// the contract with Cursor's backend; they must match the JS implementation
// exactly or upstream silently misparses the request.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"time"
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireLen     = 2
	wireFixed32 = 5
)

// Top-level StreamUnifiedChatRequestWithTools / StreamUnifiedChatRequest.
const (
	fldRequest = 1 // wrapper: StreamUnifiedChatRequest

	fldMessages         = 1
	fldUnknown2         = 2
	fldInstruction      = 3
	fldUnknown4         = 4
	fldModel            = 5
	fldWebTool          = 8
	fldUnknown13        = 13
	fldCursorSetting    = 15
	fldUnknown19        = 19
	fldConversationID   = 23
	fldMetadata         = 26
	fldIsAgentic        = 27
	fldSupportedTools   = 29
	fldMessageIDs       = 30
	fldMcpTools         = 34
	fldLargeContext     = 35
	fldUnknown38        = 38
	fldUnifiedMode      = 46
	fldUnknown47        = 47
	fldDisableTools     = 48
	fldThinkingLevel    = 49
	fldUnknown51        = 51
	fldUnknown53        = 53
	fldUnifiedModeName  = 54
	fldToolResultUpload = 2 // client_side_tool_v2_result (separate frame, unused here)
)

// ConversationMessage.
const (
	msgContent       = 1
	msgRole          = 2
	msgID            = 13
	msgToolResults   = 18
	msgIsAgentic     = 29
	msgServerBubble  = 32
	msgUnifiedMode   = 47
	msgSupportedTool = 51
)

// StreamUnifiedChatResponseWithTools.
const (
	respToolCall = 1
	respResponse = 2
)

// ClientSideToolV2Call (both request mcp_tools and response tool_call shapes).
const (
	toolID       = 3
	toolName     = 9
	toolRawArgs  = 10
	toolIsLast   = 11
	toolMcpParam = 27
)

// MCPParams / nested tool.
const (
	mcpToolsList    = 1
	mcpNestedName   = 1
	mcpNestedParams = 3
)

// StreamUnifiedChatResponse.
const (
	respText     = 1
	respThinking = 25
)

// Cursor model / metadata / message-id subfields.
const (
	modelName       = 1
	modelEmpty      = 4
	instructionText = 1
	settingPath     = 1
	settingUnknown3 = 3
	settingUnknown6 = 6
	settingUnknown8 = 8
	settingUnknown9 = 9
	metaPlatform    = 1
	metaArch        = 2
	metaVersion     = 3
	metaCwd         = 4
	metaTimestamp   = 5
	msgidID         = 1
	msgidRole       = 3
	mcpToolName     = 1
	mcpToolDesc     = 2
	mcpToolParams   = 3
	mcpToolServer   = 4
	thinkingText    = 1
)

// Roles / modes / thinking levels.
const (
	roleUser      = 1
	roleAssistant = 2

	unifiedChat  = 1
	unifiedAgent = 2

	thinkingUnspecified = 0
	thinkingMedium      = 1
	thinkingHigh        = 2
)

// ==================== primitive encoding ====================

func encodeVarint(value uint64) []byte {
	var out []byte
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func concat(chunks ...[]byte) []byte {
	var size int
	for _, c := range chunks {
		size += len(c)
	}
	out := make([]byte, 0, size)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func fieldVarint(field int, value uint64) []byte {
	return concat(encodeVarint(uint64(field)<<3|wireVarint), encodeVarint(value))
}

func fieldBytes(field int, value []byte) []byte {
	if value == nil {
		value = []byte{}
	}
	return concat(encodeVarint(uint64(field)<<3|wireLen), encodeVarint(uint64(len(value))), value)
}

func fieldString(field int, value string) []byte {
	return fieldBytes(field, []byte(value))
}

// ==================== request building ====================

// cursorToolCall is the OpenAI-shaped tool call carried in messages.
type cursorToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// cursorTool is one function definition sent as an MCP tool.
type cursorTool struct {
	Name        string
	Description string
	Arguments   string
}

// cursorMessage is the pre-translation message shape encodeChatRequest takes.
// tool role messages have already been folded into Content as text blocks by
// translate.go (9router's openai-to-cursor does the same, to avoid Cursor's
// protobuf tool_results loop on schema mismatches).
type cursorMessage struct {
	Role    string // "user" | "assistant" | "system"
	Content string
}

// encodeCursorModel encodes the Model message.
// encodeInstruction encodes the instruction sub-message; an empty instruction
// is an empty payload, not a nested empty field.
func encodeInstruction(text string) []byte {
	if text == "" {
		return nil
	}
	return fieldString(instructionText, text)
}

func encodeCursorModel(name string) []byte {
	return concat(
		fieldString(modelName, name),
		fieldBytes(modelEmpty, nil),
	)
}

func encodeCursorSetting() []byte {
	unknown6 := concat(
		fieldBytes(1, nil),
		fieldBytes(2, nil),
	)
	return concat(
		fieldString(settingPath, `cursor\aisettings`),
		fieldBytes(settingUnknown3, nil),
		fieldBytes(settingUnknown6, unknown6),
		fieldVarint(settingUnknown8, 1),
		fieldVarint(settingUnknown9, 1),
	)
}

// encodeMetadata encodes the client metadata block. Version/cwd mirror what
// the Node client sends; Cursor does not validate them beyond shape.
func encodeMetadata() []byte {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "/"
	}
	return concat(
		fieldString(metaPlatform, clientOS()),
		fieldString(metaArch, clientArch()),
		fieldString(metaVersion, "v24.15.0"),
		fieldString(metaCwd, cwd),
		fieldString(metaTimestamp, timeNowISO()),
	)
}

func encodeMessageID(id string, role int) []byte {
	return concat(
		fieldString(msgidID, id),
		fieldVarint(msgidRole, uint64(role)),
	)
}

// encodeConversationMessage encodes one ConversationMessage. Per 9router, the
// message-level unified_mode mirrors whether the turn carries tools.
func encodeConversationMessage(content string, role int, messageID string, hasTools, isLast bool) []byte {
	messageMode := unifiedChat
	if hasTools {
		messageMode = unifiedAgent
	}
	out := concat(
		fieldString(msgContent, content),
		fieldVarint(msgRole, uint64(role)),
		fieldString(msgID, messageID),
		fieldVarint(msgIsAgentic, boolToVarint(hasTools)),
		fieldVarint(msgUnifiedMode, uint64(messageMode)),
	)
	if isLast && hasTools {
		out = concat(out, fieldBytes(msgSupportedTool, encodeVarint(1)))
	}
	return out
}

// encodeMcpTool encodes one MCPTool from an OpenAI function tool definition.
func encodeMcpTool(name, description, schemaJSON string) []byte {
	out := []byte{}
	if name != "" {
		out = concat(out, fieldString(mcpToolName, name))
	}
	if description != "" {
		out = concat(out, fieldString(mcpToolDesc, description))
	}
	if schemaJSON != "" && schemaJSON != "{}" {
		out = concat(out, fieldString(mcpToolParams, schemaJSON))
	}
	return concat(out, fieldString(mcpToolServer, "custom"))
}

// encodeChatRequest builds the StreamUnifiedChatRequest body (9router
// encodeRequest). forceAgentMode mirrors the Claude Code UA handling.
func encodeChatRequest(messages []cursorMessage, modelName string, tools []cursorTool, reasoningEffort string, forceAgentMode bool) []byte {
	hasTools := len(tools) > 0
	isAgentic := hasTools || forceAgentMode

	// System messages become user turns prefixed like 9router's translator.
	normalized := make([]cursorMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			content := "[System Instructions]\n" + m.Content
			normalized = append(normalized, cursorMessage{Role: "user", Content: content})
		} else {
			normalized = append(normalized, cursorMessage{Role: m.Role, Content: m.Content})
		}
	}

	var out []byte
	messageIDs := make([]string, 0, len(normalized))
	for i, m := range normalized {
		role := roleAssistant
		if m.Role == "user" {
			role = roleUser
		}
		id := uuidFactory()
		messageIDs = append(messageIDs, id)
		out = concat(out, fieldBytes(fldMessages, encodeConversationMessage(m.Content, role, id, hasTools, i == len(normalized)-1)))
	}

	thinkingLevel := uint64(thinkingUnspecified)
	switch reasoningEffort {
	case "medium":
		thinkingLevel = thinkingMedium
	case "high":
		thinkingLevel = thinkingHigh
	}

	out = concat(out,
		fieldVarint(fldUnknown2, 1),
		fieldBytes(fldInstruction, encodeInstruction("")),
		fieldVarint(fldUnknown4, 1),
		fieldBytes(fldModel, encodeCursorModel(modelName)),
		fieldString(fldWebTool, ""),
		fieldVarint(fldUnknown13, 1),
		fieldBytes(fldCursorSetting, encodeCursorSetting()),
		fieldVarint(fldUnknown19, 1),
		fieldString(fldConversationID, uuidFactory()),
		fieldBytes(fldMetadata, encodeMetadata()),
		fieldVarint(fldIsAgentic, boolToVarint(isAgentic)),
	)
	if isAgentic {
		out = concat(out, fieldBytes(fldSupportedTools, encodeVarint(1)))
	}
	for i, id := range messageIDs {
		role := roleAssistant
		if normalized[i].Role == "user" {
			role = roleUser
		}
		out = concat(out, fieldBytes(fldMessageIDs, encodeMessageID(id, role)))
	}
	for _, t := range tools {
		out = concat(out, fieldBytes(fldMcpTools, encodeMcpTool(t.Name, t.Description, t.Arguments)))
	}

	unifiedMode := unifiedChat
	unifiedModeName := "Ask"
	if isAgentic {
		unifiedMode = unifiedAgent
		unifiedModeName = "Agent"
	}
	out = concat(out,
		fieldVarint(fldLargeContext, 0),
		fieldVarint(fldUnknown38, 0),
		fieldVarint(fldUnifiedMode, uint64(unifiedMode)),
		fieldString(fldUnknown47, ""),
		fieldVarint(fldDisableTools, boolToVarint(!isAgentic)),
		fieldVarint(fldThinkingLevel, thinkingLevel),
		fieldVarint(fldUnknown51, 0),
		fieldVarint(fldUnknown53, 1),
		fieldString(fldUnifiedModeName, unifiedModeName),
	)
	return out
}

// buildChatRequest wraps encodeChatRequest in the top-level request field.
func buildChatRequest(messages []cursorMessage, modelName string, tools []cursorTool, reasoningEffort string, forceAgentMode bool) []byte {
	return fieldBytes(fldRequest, encodeChatRequest(messages, modelName, tools, reasoningEffort, forceAgentMode))
}

// ==================== ConnectRPC framing ====================

// wrapConnectFrame frames a protobuf payload: 1 flag byte + 4-byte big-endian
// length. Cursor rejects compressed requests, so compress stays false.
func wrapConnectFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x00
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// decompressFramePayload mirrors decompressPayload: gzip first, then zlib,
// then raw deflate; JSON error frames and unknown flags pass through.
func decompressFramePayload(payload []byte, flags byte) []byte {
	if len(payload) > 10 && payload[0] == 0x7b && payload[1] == 0x22 {
		// starts with {" — an uncompressed JSON error frame
		return payload
	}
	const (
		flagGzip    = 0x01
		flagTrailer = 0x02
	)
	if flags == flagGzip || flags == flagTrailer || flags == flagGzip|flagTrailer {
		if out, err := gunzip(payload); err == nil {
			return out
		}
		if out, err := inflate(payload); err == nil {
			return out
		}
		if out, err := inflateRaw(payload); err == nil {
			return out
		}
	}
	return payload
}

func gunzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

func inflate(data []byte) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

func inflateRaw(data []byte) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(data))
	return io.ReadAll(reader)
}

// ==================== primitive decoding ====================

// pbField is one decoded protobuf field occurrence.
type pbField struct {
	Number int
	Value  []byte // LEN payload; VARINT value re-encoded below
	Varint uint64
	IsLen  bool
}

// decodeMessage parses all fields, keeping every occurrence in order.
func decodeMessage(data []byte) []pbField {
	var fields []pbField
	pos := 0
	for pos < len(data) {
		tag, newPos, ok := readVarint(data, pos)
		if !ok {
			break
		}
		pos = newPos
		number := int(tag >> 3)
		wire := int(tag & 0x07)
		switch wire {
		case wireVarint:
			value, next, ok := readVarint(data, pos)
			if !ok {
				return fields
			}
			pos = next
			fields = append(fields, pbField{Number: number, Varint: value})
		case wireLen:
			length, next, ok := readVarint(data, pos)
			if !ok || length > uint64(len(data)-next) {
				return fields
			}
			pos = next
			fields = append(fields, pbField{Number: number, Value: data[pos : pos+int(length)], IsLen: true})
			pos += int(length)
		case wireFixed64:
			if pos+8 > len(data) {
				return fields
			}
			fields = append(fields, pbField{Number: number, Varint: binary.LittleEndian.Uint64(data[pos : pos+8])})
			pos += 8
		case wireFixed32:
			if pos+4 > len(data) {
				return fields
			}
			fields = append(fields, pbField{Number: number, Varint: uint64(binary.LittleEndian.Uint32(data[pos : pos+4]))})
			pos += 4
		default:
			return fields
		}
	}
	return fields
}

func readVarint(data []byte, offset int) (uint64, int, bool) {
	var result uint64
	var shift uint
	pos := offset
	for pos < len(data) {
		b := data[pos]
		pos++
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, pos, true
		}
		shift += 7
		if shift > 63 {
			return 0, pos, false
		}
	}
	return result, pos, false
}

func fieldFirst(fields []pbField, number int) (pbField, bool) {
	for _, f := range fields {
		if f.Number == number {
			return f, true
		}
	}
	return pbField{}, false
}

func fieldStringFirst(fields []pbField, number int) string {
	if f, ok := fieldFirst(fields, number); ok && f.IsLen {
		return string(f.Value)
	}
	return ""
}

// ==================== response parsing ====================

// chatResult is what one response frame contributes.
type chatResult struct {
	Text     string
	Thinking string
	ToolCall *toolCallResult
	Error    string
}

type toolCallResult struct {
	ID        string
	Name      string
	Arguments string
	IsLast    bool
}

// extractToolCall mirrors extractToolCall: nested MCP params override the
// top-level name/raw_args.
func extractToolCall(data []byte) *toolCallResult {
	fields := decodeMessage(data)
	id := ""
	if f, ok := fieldFirst(fields, toolID); ok && f.IsLen {
		full := string(f.Value)
		// Cursor returns a multi-line id; the first line is the call id.
		for i := 0; i < len(full); i++ {
			if full[i] == '\n' {
				full = full[:i]
				break
			}
		}
		id = full
	}
	name := fieldStringFirst(fields, toolName)
	args := ""
	isLast := false
	if f, ok := fieldFirst(fields, toolIsLast); ok {
		isLast = f.Varint != 0
	}
	if f, ok := fieldFirst(fields, toolMcpParam); ok && f.IsLen {
		mcp := decodeMessage(f.Value)
		if list, ok := fieldFirst(mcp, mcpToolsList); ok && list.IsLen {
			tool := decodeMessage(list.Value)
			if n := fieldStringFirst(tool, mcpNestedName); n != "" {
				name = n
			}
			if p, ok := fieldFirst(tool, mcpNestedParams); ok && p.IsLen {
				args = string(p.Value)
			}
		}
	}
	if args == "" {
		args = fieldStringFirst(fields, toolRawArgs)
	}
	if id == "" || name == "" {
		return nil
	}
	if args == "" {
		args = "{}"
	}
	return &toolCallResult{ID: id, Name: name, Arguments: args, IsLast: isLast}
}

// extractChatResult mirrors extractTextFromResponse for one frame payload.
// Cursor also streams JSON error frames; those are recognized up front.
func extractChatResult(payload []byte) chatResult {
	if len(payload) > 0 && payload[0] == 0x7b {
		if msg := parseCursorErrorFrame(payload); msg != "" {
			return chatResult{Error: msg}
		}
	}
	fields := decodeMessage(payload)
	if f, ok := fieldFirst(fields, respToolCall); ok && f.IsLen {
		if tc := extractToolCall(f.Value); tc != nil {
			return chatResult{ToolCall: tc}
		}
	}
	if f, ok := fieldFirst(fields, respResponse); ok && f.IsLen {
		nested := decodeMessage(f.Value)
		text := fieldStringFirst(nested, respText)
		thinking := ""
		if tf, ok := fieldFirst(nested, respThinking); ok && tf.IsLen {
			thinking = fieldStringFirst(decodeMessage(tf.Value), thinkingText)
		}
		if text != "" || thinking != "" {
			return chatResult{Text: text, Thinking: thinking}
		}
	}
	return chatResult{}
}

// parseCursorErrorFrame decodes {"error":{...}} frames the way
// createErrorResponse does: prefer the debug title/detail, then the message.
func parseCursorErrorFrame(payload []byte) string {
	var frame struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug *struct {
					Details *struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil || frame.Error == nil {
		return ""
	}
	for _, detail := range frame.Error.Details {
		if detail.Debug != nil && detail.Debug.Details != nil {
			if detail.Debug.Details.Title != "" {
				return detail.Debug.Details.Title
			}
			if detail.Debug.Details.Detail != "" {
				return detail.Debug.Details.Detail
			}
		}
	}
	if frame.Error.Message != "" {
		return frame.Error.Message
	}
	return frame.Error.Code
}

// ==================== AgentService frames ====================

// agentString/agentMessage helpers matching the JS agent frame builders.
func agentString(field int, value string) []byte { return fieldString(field, value) }

func agentBool(field int, value bool) []byte {
	if value {
		return fieldVarint(field, 1)
	}
	return fieldVarint(field, 0)
}

func encodeHistoryMessage(content string, role string) []byte {
	if content == "" {
		return nil
	}
	text := agentString(1, content)
	if role == "assistant" {
		return fieldBytes(2, fieldBytes(1, fieldBytes(1, text)))
	}
	return fieldBytes(1, fieldBytes(1, fieldBytes(1, text)))
}

// buildAgentRunFrame mirrors buildAgentRunFrame: the agent.v1 run_request
// client message carrying the current user turn plus optional history.
func buildAgentRunFrame(messages []cursorMessage, model string, contexts ...*agentContext) []byte {
	var chat []cursorMessage
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		chat = append(chat, m)
	}
	currentIndex := -1
	for i, m := range chat {
		if m.Role == "user" {
			currentIndex = i
		}
	}
	var history [][]byte
	if currentIndex >= 0 {
		for _, m := range chat[:currentIndex] {
			if encoded := encodeHistoryMessage(m.Content, m.Role); encoded != nil {
				history = append(history, encoded)
			}
		}
	} else if len(chat) > 0 {
		for _, m := range chat[:len(chat)-1] {
			if encoded := encodeHistoryMessage(m.Content, m.Role); encoded != nil {
				history = append(history, encoded)
			}
		}
	}
	current := cursorMessage{Role: "user", Content: "Continue."}
	if currentIndex >= 0 {
		current = chat[currentIndex]
	} else if len(chat) > 0 {
		current = chat[len(chat)-1]
	}
	if current.Content == "" {
		current.Content = "Continue."
	}

	userMessage := concat(
		agentString(1, current.Content),
		agentString(2, uuidFactory()),
	)
	var userAction []byte
	if len(history) > 0 && (len(contexts) == 0 || contexts[0] == nil) {
		var historyFields []byte
		for _, entry := range history {
			historyFields = concat(historyFields, fieldBytes(1, entry))
		}
		userAction = concat(fieldBytes(1, userMessage), fieldBytes(7, historyFields))
	} else {
		userAction = fieldBytes(1, userMessage)
	}
	conversationAction := fieldBytes(1, userAction)
	requestedModel := concat(agentString(1, model), agentBool(7, true))
	var state []byte
	if len(contexts) > 0 && contexts[0] != nil {
		state = contexts[0].state
	}
	runRequest := concat(
		fieldBytes(1, state),
		fieldBytes(2, conversationAction),
	)
	if len(contexts) > 0 && contexts[0] != nil {
		runRequest = concat(runRequest, fieldString(5, uuidFactory()))
	}
	runRequest = concat(runRequest, fieldBytes(9, requestedModel))
	return wrapConnectFrame(fieldBytes(1, runRequest))
}

// createRequestContextResponse answers AgentService's exec_request with an
// empty RequestContext (this plugin has no IDE file context).
func createRequestContextResponse() []byte {
	requestContextSuccess := fieldBytes(1, nil)
	requestContextResult := fieldBytes(1, requestContextSuccess)
	execClientMessage := fieldBytes(10, requestContextResult)
	return wrapConnectFrame(fieldBytes(2, execClientMessage))
}

// agentUpdate is one decoded interaction_update event.
type agentUpdate struct {
	TextDelta string
	Finished  bool
}

// decodeAgentServerMessage parses one AgentServerMessage payload.
// execRequestField10 reports whether the message is a RequestContext request
// (the only exec variant this plugin can service).
func decodeAgentServerMessage(payload []byte) (update agentUpdate, execRequest bool, execSupported bool) {
	fields := decodeMessage(payload)
	if f, ok := fieldFirst(fields, 1); ok && f.IsLen {
		updateFields := decodeMessage(f.Value)
		if uf, ok := fieldFirst(updateFields, 1); ok && uf.IsLen {
			textFields := decodeMessage(uf.Value)
			update.TextDelta = fieldStringFirst(textFields, 1)
		}
		if _, ok := fieldFirst(updateFields, 14); ok {
			update.Finished = true
		}
	}
	if f, ok := fieldFirst(fields, 2); ok && f.IsLen {
		execRequest = true
		execFields := decodeMessage(f.Value)
		_, execSupported = fieldFirst(execFields, 10)
	}
	return update, execRequest, execSupported
}

func joinNonEmpty(parts []string, sep string) string {
	var out string
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

func boolToVarint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// Determinism hooks for the 9router conformance test: uuid/time output is the
// only non-reproducible part of the wire encoding.
var (
	uuidFactory = randomUUID
	timeNowISO  = isoNow
)
