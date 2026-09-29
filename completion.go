package main

// Non-streaming aggregation: the executor only ever speaks stream internally,
// so a plain chat.completion is rebuilt from the chunks the emitter produced.

import (
	"encoding/json"
	"strings"
)

type completionAggregator struct {
	model     string
	id        string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []map[string]any
	finish    string
	usage     map[string]any
}

func newCompletionAggregator(model string) *completionAggregator {
	return &completionAggregator{model: model}
}

// consume receives the chunk payloads the emitter would stream (unframed).
func (a *completionAggregator) consume(chunk []byte) error {
	data := strings.TrimSpace(string(chunk))
	data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
	if data == "" || data == "[DONE]" {
		return nil
	}
	var parsed struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return nil // tolerate stray frames
	}
	if parsed.ID != "" {
		a.id = parsed.ID
	}
	if parsed.Created != 0 {
		a.created = parsed.Created
	}
	if parsed.Usage != nil {
		a.usage = map[string]any{}
		for k, v := range parsed.Usage {
			a.usage[k] = v
		}
	}
	for _, choice := range parsed.Choices {
		if choice.Delta.Content != "" {
			a.content.WriteString(choice.Delta.Content)
		}
		if choice.Delta.ReasoningContent != "" {
			a.reasoning.WriteString(choice.Delta.ReasoningContent)
		}
		for _, tc := range choice.Delta.ToolCalls {
			for len(a.toolCalls) <= tc.Index {
				a.toolCalls = append(a.toolCalls, map[string]any{})
			}
			entry := a.toolCalls[tc.Index]
			if tc.ID != "" {
				entry["id"] = tc.ID
			}
			if tc.Type != "" {
				entry["type"] = tc.Type
			}
			fn, _ := entry["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
			}
			if tc.Function.Name != "" {
				fn["name"] = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				fn["arguments"] = tc.Function.Arguments
			}
			entry["function"] = fn
			a.toolCalls[tc.Index] = entry
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finish = *choice.FinishReason
		}
	}
	return nil
}

// completion renders the folded chat.completion.
func (a *completionAggregator) completion() ([]byte, error) {
	if a.id == "" {
		a.id = "chatcmpl-msg_completion"
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	message := map[string]any{
		"role":    "assistant",
		"content": a.content.String(),
	}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		message["tool_calls"] = a.toolCalls
		if a.content.String() == "" {
			message["content"] = nil
		}
	}
	return json.Marshal(map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": a.usage,
	})
}
