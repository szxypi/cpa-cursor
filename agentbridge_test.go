package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func testBridgeStore(t *testing.T) (*agentBridgeStore, *preparedChat, *agentBridgeSession, *agentToolRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &agentBridgeSession{ctx: ctx, cancel: cancel, client: &agentClient{}, events: make(chan agentBridgeEvent, 8)}
	store := &agentBridgeStore{calls: make(map[string]*agentPendingCall), now: time.Now, ttl: time.Hour, limit: 2}
	t.Cleanup(store.shutdown)
	t.Cleanup(s.close)
	p := &preparedChat{binding: [32]byte{42}, parsed: &parsedChat{RawMessages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"read fixture"`)}}, Tools: []cursorTool{{Name: "Read", Arguments: `{"type":"object"}`}}}}
	tool := &agentToolRequest{Name: "Read", Arguments: `{"file_path":"fixture.txt"}`}
	return store, p, s, tool
}
func toolContinuation(p *preparedChat, id string, tool *agentToolRequest, text, result string) *preparedChat {
	raw, _ := json.Marshal(p.parsed)
	var parsed parsedChat
	json.Unmarshal(raw, &parsed)
	call := openAIToolCall{ID: id, Type: "function"}
	call.Function.Name = tool.Name
	call.Function.Arguments = tool.Arguments
	content, _ := json.Marshal(text)
	output, _ := json.Marshal(result)
	parsed.RawMessages = append(parsed.RawMessages, openAIMessage{Role: "assistant", Content: content, ToolCalls: []openAIToolCall{call}}, openAIMessage{Role: "tool", Content: output, ToolCallID: id})
	return &preparedChat{binding: p.binding, parsed: &parsed}
}
func TestBridgeExactlyOnceAndReplay(t *testing.T) {
	b, p, s, tool := testBridgeStore(t)
	id := "call_cpa_synthetic"
	if err := b.park(id, s, tool, p, "reading"); err != nil {
		t.Fatal(err)
	}
	continuation := toolContinuation(p, id, tool, "reading", "random-result-931")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	var winner *agentPendingCall
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call, replay, err := b.claim(id, continuation)
			if err == nil && !replay {
				mu.Lock()
				wins++
				winner = call
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("claimed %d times", wins)
	}
	b.complete(winner, []agentBridgeEvent{{Text: "observed-result"}}, nil)
	call, replay, err := b.claim(id, continuation)
	if err != nil || !replay || call.replay[0].Text != "observed-result" {
		t.Fatal("exact duplicate did not replay")
	}
	tampered := toolContinuation(p, id, tool, "reading", "different-result")
	if _, _, err = b.claim(id, tampered); err == nil {
		t.Fatal("changed result was replayed")
	}
}

// 不匹配的续接一律交由调用方用完整历史重开；只有同一调用方的改写会释放旧会话，
// 其他身份拿到相同 id 也不能影响原会话。
func TestBridgeMismatchFallsBackWithoutCrossIdentityEffects(t *testing.T) {
	for _, kind := range []string{"identity", "history", "tools", "arguments", "assistant", "missing_call"} {
		t.Run(kind, func(t *testing.T) {
			b, p, s, tool := testBridgeStore(t)
			id := "call_cpa_guarded"
			if err := b.park(id, s, tool, p, "reading"); err != nil {
				t.Fatal(err)
			}
			next := toolContinuation(p, id, tool, "reading", "content")
			switch kind {
			case "identity":
				next.binding = [32]byte{99}
			case "history":
				next.parsed.RawMessages[0].Content = json.RawMessage(`"different"`)
			case "tools":
				next.parsed.Tools[0].Name = "Bash"
			case "arguments":
				next.parsed.RawMessages[1].ToolCalls[0].Function.Arguments = `{"file_path":"other"}`
			case "assistant":
				next.parsed.RawMessages[1].Content = json.RawMessage(`"tampered"`)
			case "missing_call":
				next.parsed.RawMessages[1].ToolCalls = nil
			}
			if _, _, err := b.claim(id, next); err != errAgentContinuationLost {
				t.Fatalf("mismatched continuation: got %v, want fresh-run fallback", err)
			}
			_, _, err := b.claim(id, toolContinuation(p, id, tool, "reading", "content"))
			if kind == "identity" {
				if err != nil || s.ctx.Err() != nil {
					t.Fatal("another identity disturbed the pending continuation", err)
				}
				return
			}
			if err != errAgentContinuationLost || s.ctx.Err() == nil {
				t.Fatalf("stale upstream session was kept after the caller rewrote history: err=%v", err)
			}
		})
	}
}
func TestBridgeExpiryCapacityAndCleanup(t *testing.T) {
	b, p, s, tool := testBridgeStore(t)
	b.limit = 1
	now := time.Now()
	b.now = func() time.Time { return now }
	if err := b.park("a", s, tool, p, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.park("b", s, tool, p, ""); err == nil {
		t.Fatal("capacity exceeded")
	}
	now = now.Add(2 * time.Hour)
	if _, _, err := b.claim("a", toolContinuation(p, "a", tool, "", "result")); err == nil {
		t.Fatal("expired call accepted")
	}
	if s.ctx.Err() == nil {
		t.Fatal("expired stream not cancelled")
	}
}
func TestBridgeDiscardReplayDoesNotKillNewPendingTool(t *testing.T) {
	b, p, s, tool := testBridgeStore(t)
	if err := b.park("A", s, tool, p, ""); err != nil {
		t.Fatal(err)
	}
	call, _, err := b.claim("A", toolContinuation(p, "A", tool, "", "value"))
	if err != nil {
		t.Fatal(err)
	}
	b.complete(call, []agentBridgeEvent{{ID: "B", Tool: tool}}, nil)
	if err := b.park("B", s, tool, p, ""); err != nil {
		t.Fatal(err)
	}
	b.discard("A")
	if s.ctx.Err() != nil || b.calls["B"] == nil {
		t.Fatal("discarding completed replay killed later pending call")
	}
}

func TestOriginalToolErrorPreserved(t *testing.T) {
	got := originalToolErrors([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_x","is_error":true,"content":"denied"}]}]}`))
	if !got["call_x"] {
		t.Fatal("client tool denial lost")
	}
}
