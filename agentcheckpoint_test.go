package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// 新用户消息只有在历史与保存 checkpoint 的那一轮完全一致时才复用，且只能用一次。
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
	if again, _, _ := store.take(nextTurn(p, "Got it.", "what is my cat?")); again != nil {
		t.Fatal("checkpoint reused twice")
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
	if again, _, _ := after.take(nextTurn(p, "Got it.", "what is my cat?")); again != nil {
		t.Fatal("disk checkpoint reused twice")
	}
}

// 中间件压缩旧的工具结果/用户文本时，结构与 assistant 消息不变仍可续接；assistant 被改则不行。
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
