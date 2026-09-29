package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkpointFixture() (*preparedChat, *agentClient) {
	p := &preparedChat{binding: [32]byte{7}, parsed: &parsedChat{
		RawMessages: []openAIMessage{{Role: "user", Content: json.RawMessage(`"my cat is Momo"`)}},
		Tools:       []cursorTool{{Name: "Read", Arguments: `{"type":"object"}`}},
	}}
	client := &agentClient{checkpoint: []byte("state-bytes"), context: &agentContext{blobs: map[string][]byte{"k": []byte("v")}, conversationID: "conv-1"}}
	return p, client
}

func nextTurn(p *preparedChat, assistant string, users ...string) *preparedChat {
	ms := append([]openAIMessage(nil), p.parsed.RawMessages...)
	content, _ := json.Marshal(assistant)
	ms = append(ms, openAIMessage{Role: "assistant", Content: content})
	for _, u := range users {
		c, _ := json.Marshal(u)
		ms = append(ms, openAIMessage{Role: "user", Content: c})
	}
	return &preparedChat{binding: p.binding, parsed: &parsedChat{RawMessages: ms, Tools: p.parsed.Tools}}
}

// 新用户消息只有在历史与保存 checkpoint 的那一轮完全一致时才复用；同一 checkpoint 可多次续接。
func TestAgentCheckpointReuseRequiresExactHistory(t *testing.T) {
	store := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now}
	p, client := checkpointFixture()
	store.save(p, client, agentAssistantMessage("Got it.", nil))

	if cp, _, _ := store.take(nextTurn(p, "Something else", "what is my cat?")); cp != nil {
		t.Fatal("reused checkpoint although the assistant reply differs")
	}
	other := nextTurn(p, "Got it.", "what is my cat?")
	other.binding = [32]byte{8}
	if cp, _, _ := store.take(other); cp != nil {
		t.Fatal("reused checkpoint across callers")
	}
	cp, text, _ := store.take(nextTurn(p, "Got it.", "<system-reminder>x</system-reminder>", "what is my cat?"))
	if cp == nil || text != "<system-reminder>x</system-reminder>\n\nwhat is my cat?" {
		t.Fatalf("checkpoint not reused: %v %q", cp, text)
	}
	if again, _, _ := store.take(nextTurn(p, "Got it.", "what is my cat?")); again != cp {
		t.Fatal("checkpoint not reusable after the first take")
	}

	c := newAgentContext(nil)
	cp.apply(c, text)
	frame := buildAgentRunFrame(nil, "m", c)
	req := decodeMessage(fieldFirstValue(t, decodeMessage(frame[5:]), 1))
	if string(fieldFirstValue(t, req, 1)) != "state-bytes" || string(fieldFirstValue(t, req, 5)) != "conv-1" {
		t.Fatal("run frame does not carry the checkpoint state and conversation id")
	}
	if string(c.blobs["k"]) != "v" {
		t.Fatal("checkpoint blobs not available for KV reads")
	}
}

func fieldFirstValue(t *testing.T, fields []pbField, n int) []byte {
	t.Helper()
	f, ok := fieldFirst(fields, n)
	if !ok {
		t.Fatalf("field %d missing", n)
	}
	return f.Value
}

func TestAgentCheckpointSurvivesRestartOnDisk(t *testing.T) {
	dir := t.TempDir()

	p, client := checkpointFixture()
	before := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now, disk: dir}
	before.save(p, client, agentAssistantMessage("Got it.", nil))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if entries, _ := os.ReadDir(dir); len(entries) == 1 && filepath.Ext(entries[0].Name()) == ".ckpt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	after := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now, disk: dir}
	cp, text, miss := after.take(nextTurn(p, "Got it.", "what is my cat?"))
	if cp == nil {
		t.Fatalf("checkpoint not loaded lazily: %s", miss)
	}
	before.save(p, client, agentAssistantMessage("Got it.", nil))
	time.Sleep(200 * time.Millisecond)
	after = &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now, disk: dir}
	after.loadAllDisk(dir)
	if len(after.m) != 1 {
		t.Fatalf("startup load found %d checkpoints", len(after.m))
	}
	cp, text, miss = after.take(nextTurn(p, "Got it.", "what is my cat?"))
	if cp == nil || text != "what is my cat?" || string(cp.state) != "state-bytes" || string(cp.blobs["k"]) != "v" {
		t.Fatalf("checkpoint not restored from disk: %v %q %s", cp, text, miss)
	}
	if again, _, _ := after.take(nextTurn(p, "Got it.", "what is my cat?")); again == nil {
		t.Fatal("disk checkpoint not reusable")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("checkpoint file removed on take: %d files", len(entries))
	}
}

// 中间件压缩旧的工具结果/用户文本或旧调用入参时，结构与 assistant 正文不变仍可续接；正文被改则不行。
func TestAgentCheckpointRelaxedMatchToleratesRewrittenToolText(t *testing.T) {
	store := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now}
	p, client := checkpointFixture()
	call := openAIToolCall{ID: "call_1", Type: "function"}
	call.Function.Name, call.Function.Arguments = "Read", `{"path":"a"}`
	history := append(p.parsed.RawMessages,
		openAIMessage{Role: "assistant", Content: json.RawMessage(`"reading"`), ToolCalls: []openAIToolCall{call}},
		openAIMessage{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"a very long original file body"`)},
		openAIMessage{Role: "user", Content: json.RawMessage(`"summarize it"`)})
	p.parsed.RawMessages = history
	store.save(p, client, agentAssistantMessage("Got it.", nil))

	rewritten := func(assistant string) *preparedChat {
		next := nextTurn(p, "Got it.", "and now?")
		next.parsed.RawMessages[1].Content = json.RawMessage(assistant)
		next.parsed.RawMessages[2].Content = json.RawMessage(`"[paged out]"`)
		paged := call
		paged.Function.Arguments = `{"path":"[paged out]"}`
		next.parsed.RawMessages[1].ToolCalls = []openAIToolCall{paged}
		return next
	}
	if cp, _, _ := store.take(rewritten(`"reading!"`)); cp != nil {
		t.Fatal("reused checkpoint although an assistant message changed")
	}
	cp, text, relaxed := store.take(rewritten(`"reading"`))
	if cp == nil || relaxed != "-relaxed" || text != "and now?" {
		t.Fatalf("relaxed match failed: %v %q %q", cp, text, relaxed)
	}
}

// hook 改写本轮调用入参、中间件改写更早历史时，按本插件生成的工具调用 ID 仍可续接；
// 外来 ID 与其他调用方不参与该匹配。
func TestAgentCheckpointMatchesByToolCallIDs(t *testing.T) {
	store := &agentCheckpointStore{m: map[[32]byte]*agentCheckpoint{}, now: agentCheckpoints.now}
	p, client := checkpointFixture()
	calls := make([]openAIToolCall, 2)
	for i := range calls {
		calls[i] = openAIToolCall{ID: fmt.Sprintf("%s%d", agentToolIDPrefix, i), Type: "function"}
		calls[i].Function.Name, calls[i].Function.Arguments = "Bash", `{"command":"ls"}`
	}
	store.save(p, client, agentAssistantMessage("checking", calls))

	next := func(prefix string, binding [32]byte) *preparedChat {
		ms := append([]openAIMessage(nil), p.parsed.RawMessages...)
		ms[0].Content = json.RawMessage(`"[paged out]"`)
		echoed := make([]openAIToolCall, len(calls))
		for i, c := range calls {
			echoed[i] = c
			echoed[i].ID = strings.Replace(c.ID, agentToolIDPrefix, prefix, 1)
			echoed[i].Function.Arguments = `{"command":"timeout 1800 ls"}`
		}
		ms = append(ms, openAIMessage{Role: "assistant", Content: json.RawMessage(`"checking"`), ToolCalls: echoed})
		for i := len(echoed) - 1; i >= 0; i-- {
			ms = append(ms, openAIMessage{Role: "tool", ToolCallID: echoed[i].ID, Content: json.RawMessage(`"out"`)})
		}
		ms = append(ms, openAIMessage{Role: "user", Content: json.RawMessage(`"<system-reminder>r</system-reminder>"`)})
		return &preparedChat{binding: binding, parsed: &parsedChat{RawMessages: ms, Tools: p.parsed.Tools}}
	}
	cp, text, match := store.take(next(agentToolIDPrefix, p.binding))
	if cp == nil || match != "-calls" || !strings.HasSuffix(text, "<system-reminder>r</system-reminder>") {
		t.Fatalf("call-id match failed: %v %q %q", cp, text, match)
	}
	if again, _, _ := store.take(next(agentToolIDPrefix, p.binding)); again != cp {
		t.Fatal("call-id match not repeatable")
	}
	if cp, _, _ := store.take(next("call_other_", p.binding)); cp != nil {
		t.Fatal("matched by foreign tool call ids")
	}
	if cp, _, _ := store.take(next(agentToolIDPrefix, [32]byte{9})); cp != nil {
		t.Fatal("matched by tool call ids across callers")
	}
}
