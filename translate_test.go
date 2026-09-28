package main

import "testing"

func TestParseChatRequestToolResultAsText(t *testing.T) {
	payload := []byte(`{
	  "model": "claude-4.5-sonnet",
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": "", "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "name": "read_file", "content": "file body"}
	  ],
	  "tools": [
	    {"type": "function", "function": {"name": "read_file", "description": "read", "parameters": {"type": "object"}}}
	  ],
	  "reasoning_effort": "high",
	  "stream": true
	}`)
	parsed, err := parseChatRequest(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages = %d", len(parsed.Messages))
	}
	// assistant tool_calls must NOT become a visible text block
	if parsed.Messages[1].Content != "" {
		t.Fatalf("assistant content = %q", parsed.Messages[1].Content)
	}
	// the tool result becomes an XML text block in the following user message
	if parsed.Messages[2].Role != "user" {
		t.Fatalf("tool role mapped to %q", parsed.Messages[2].Role)
	}
	if !containsAll(parsed.Messages[2].Content,
		"<tool_result>", "<tool_name>read_file</tool_name>",
		"<tool_call_id>call_1</tool_call_id>", "<result>file body</result>") {
		t.Fatalf("tool result block = %q", parsed.Messages[2].Content)
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %+v", parsed.Tools)
	}
	if parsed.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q", parsed.ReasoningEffort)
	}
	// tool presence forces the ChatService (agentic) path
	if parsed.AgentEligible {
		t.Fatalf("tool conversation must not take the agent path")
	}
}

func TestIsAgentTextRequestPlainText(t *testing.T) {
	payload := []byte(`{"model":"composer-1","messages":[
	  {"role":"system","content":"sys"},
	  {"role":"user","content":"hello"}],
	  "tools":[{"type":"function","function":{"name":"t","description":"","parameters":{"type":"object"}}}]}`)
	parsed, err := parseChatRequest(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.AgentEligible {
		t.Fatalf("plain text must take the agent path even with tool schemas")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !stringsContains(s, part) {
			return false
		}
	}
	return true
}

func stringsContains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
