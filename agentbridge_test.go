package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginalToolErrorPreserved(t *testing.T) {
	got := originalToolErrors([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_x","is_error":true,"content":"denied"}]}]}`))
	if !got["call_x"] {
		t.Fatal("client tool denial lost")
	}
}

func toolCallFixture(id, name, args string) openAIToolCall {
	call := openAIToolCall{ID: id, Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = args
	return call
}

// 交给客户端的工具调用结束后，客户端回传的工具结果（含附带的提醒消息）要命中同一 checkpoint，
// 并作为新用户消息发送；参数的 JSON 格式差异不影响匹配。
func TestCheckpointResumesWithHandedOffToolResults(t *testing.T) {
	store := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now}
	p, client := checkpointFixture()
	p.toolErrors = map[string]bool{"call_cpa_b": true}
	calls := []openAIToolCall{toolCallFixture("call_cpa_a", "Read", `{"file_path":"a.txt"}`), toolCallFixture("call_cpa_b", "Bash", `{"command":"ls"}`)}
	store.save(p, client, agentAssistantMessage("checking.Done", calls))

	echoed := []openAIToolCall{toolCallFixture("call_cpa_a", "Read", `{ "file_path" : "a.txt" }`), calls[1]}
	next := &preparedChat{binding: p.binding, toolErrors: p.toolErrors, parsed: &parsedChat{Tools: p.parsed.Tools, RawMessages: append(append([]openAIMessage(nil), p.parsed.RawMessages...),
		openAIMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"checking."},{"type":"text","text":"Done"}]`), ToolCalls: echoed},
		openAIMessage{Role: "tool", ToolCallID: "call_cpa_b", Content: json.RawMessage(`"permission denied"`)},
		openAIMessage{Role: "tool", ToolCallID: "call_cpa_a", Content: json.RawMessage(`"alpha"`)},
		openAIMessage{Role: "user", Content: json.RawMessage(`"<system-reminder>r</system-reminder>"`)},
	)}}
	cp, text, miss := store.take(next)
	if cp == nil {
		t.Fatalf("checkpoint not reused: %s", miss)
	}
	for _, want := range []string{`<tool_result name="Read" id="call_cpa_a">` + "\nalpha\n", `<tool_result name="Bash" id="call_cpa_b" is_error="true">` + "\npermission denied\n", "<system-reminder>r</system-reminder>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("resume text missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "call_cpa_a") > strings.Index(text, "call_cpa_b") {
		t.Fatal("tool results must follow the assistant's call order")
	}
}

// 缺少任何一个工具结果时不能续接，否则上游会把占位结果当成真实结果。
func TestCheckpointRequiresEveryToolResult(t *testing.T) {
	store := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now}
	p, client := checkpointFixture()
	calls := []openAIToolCall{toolCallFixture("call_cpa_a", "Read", `{}`), toolCallFixture("call_cpa_b", "Read", `{}`)}
	store.save(p, client, agentAssistantMessage("", calls))
	next := &preparedChat{binding: p.binding, parsed: &parsedChat{Tools: p.parsed.Tools, RawMessages: append(append([]openAIMessage(nil), p.parsed.RawMessages...),
		openAIMessage{Role: "assistant", ToolCalls: calls},
		openAIMessage{Role: "tool", ToolCallID: "call_cpa_a", Content: json.RawMessage(`"alpha"`)},
	)}}
	if cp, _, miss := store.take(next); cp != nil || miss != "tool_results" {
		t.Fatalf("partial tool results reused checkpoint (miss=%q)", miss)
	}
}
