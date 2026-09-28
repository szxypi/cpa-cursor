package main

// Client body → cursor message translation, following 9router's
// openai-to-cursor.js: tool outputs become <tool_result> text blocks in user
// messages, assistant tool_calls are dropped from the wire (Cursor rebuilds
// the turn from the tool_result blocks; the binary tool_results field loops
// on schema drift), and system prompts become user turns.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parsedChat is the normalized request the executor works with.
type parsedChat struct {
	Messages        []cursorMessage
	Tools           []cursorTool
	ReasoningEffort string
	// AgentEligible reports whether every message is plain text (no tool
	// calls, no tool results), the shape Cursor's AgentService accepts.
	AgentEligible bool
	// InputChars feeds the chars/4 usage estimate.
	InputChars int
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Name       string           `json:"name"`
	ToolCallID string           `json:"tool_call_id"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIRequest struct {
	Messages        []openAIMessage `json:"messages"`
	Tools           []openAITool    `json:"tools"`
	ReasoningEffort string          `json:"reasoning_effort"`
}

// contentText flattens an OpenAI content value (string or parts array) to
// plain text, joining text parts with newlines.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// sanitizeToolResultText strips control characters that make Cursor's backend
// reject the request (mirrors sanitizeToolResultText).
func sanitizeToolResultText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func escapeXML(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(text)
}

// buildToolResultBlock mirrors buildToolResultBlock.
func buildToolResultBlock(toolName, toolCallID, resultText string) string {
	return strings.Join([]string{
		"<tool_result>",
		"<tool_name>" + escapeXML(firstNonEmpty(toolName, "tool")) + "</tool_name>",
		"<tool_call_id>" + escapeXML(toolCallID) + "</tool_call_id>",
		"<result>" + escapeXML(sanitizeToolResultText(resultText)) + "</result>",
		"</tool_result>",
	}, "\n")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeToolCallID keeps the first line of Cursor's multi-line ids.
func normalizeToolCallID(id string) string {
	if idx := strings.IndexByte(id, '\n'); idx >= 0 {
		return id[:idx]
	}
	return id
}

// parseChatRequest accepts the OpenAI chat body CPA hands the executor
// (Payload), falling back to the untouched client body.
func parseChatRequest(body []byte) (*parsedChat, error) {
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode chat request: %w", err)
	}

	// tool_call_id -> tool name, remembered from assistant tool calls so a
	// tool message without its own name still reports the right one.
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			name := firstNonEmpty(tc.Function.Name, "tool")
			toolNames[tc.ID] = name
			if normalized := normalizeToolCallID(tc.ID); normalized != "" {
				toolNames[normalized] = name
			}
		}
	}

	parsed := &parsedChat{AgentEligible: true}
	inputChars := 0
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			parsed.AgentEligible = false
			toolName := firstNonEmpty(m.Name, toolNames[m.ToolCallID], toolNames[normalizeToolCallID(m.ToolCallID)], "tool")
			block := buildToolResultBlock(toolName, m.ToolCallID, contentText(m.Content))
			parsed.Messages = append(parsed.Messages, cursorMessage{Role: "user", Content: block})
			inputChars += len(block)
		case "assistant":
			text := contentText(m.Content)
			if len(m.ToolCalls) > 0 {
				parsed.AgentEligible = false
			}
			// The tool calls themselves are intentionally not re-sent.
			parsed.Messages = append(parsed.Messages, cursorMessage{Role: "assistant", Content: text})
			inputChars += len(text)
		case "system", "developer":
			parsed.Messages = append(parsed.Messages, cursorMessage{Role: "system", Content: contentText(m.Content)})
			inputChars += len(contentText(m.Content))
		default:
			parsed.Messages = append(parsed.Messages, cursorMessage{Role: "user", Content: contentText(m.Content)})
			inputChars += len(contentText(m.Content))
		}
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := strings.TrimSpace(string(t.Function.Parameters))
		if schema == "" || schema == "null" {
			schema = "{}"
		}
		parsed.Tools = append(parsed.Tools, cursorTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Arguments:   schema,
		})
		inputChars += len(t.Function.Description) + len(schema)
	}
	parsed.ReasoningEffort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	parsed.InputChars = inputChars
	return parsed, nil
}

// estimateTokens gives the chars/4 approximation 9router's estimateUsage uses;
// Cursor's upstream protocol carries no usage information.
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text) + 3) / 4
}
