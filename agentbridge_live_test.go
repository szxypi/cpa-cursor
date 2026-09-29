package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 仅测试客户端在隔离目录执行 Read；插件本身不得执行读文件/命令。每次工具调用都结束上游 run，
// 后续请求应以 checkpoint 续接。
func TestAgentLiveToolRoundTrip(t *testing.T) { runAgentLiveToolLoop(t, true) }

// 每次都丢弃 checkpoint，验证无法续接时以完整历史重开仍能完成任务。
func TestAgentLiveToolFreshFallback(t *testing.T) { runAgentLiveToolLoop(t, false) }

func runAgentLiveToolLoop(t *testing.T, resume bool) {
	credential := os.Getenv("CURSOR_PROBE_CREDENTIAL")
	if credential == "" {
		t.Skip("explicit live probe credential required")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	var nonce [16]byte
	rand.Read(nonce[:])
	marker := "fixture_" + hex.EncodeToString(nonce[:])
	if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	tool := openAITool{Type: "function"}
	tool.Function.Name = "Read"
	tool.Function.Description = "Read a UTF-8 file from the client computer. Use this client tool instead of builtin IDE tools."
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"],"additionalProperties":false}`)
	second := openAITool{Type: "function"}
	second.Function.Name = "stamp_result"
	second.Function.Description = "Submit the marker returned by Read and obtain a new random receipt."
	second.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"marker":{"type":"string"}},"required":["marker"],"additionalProperties":false}`)
	prompt, _ := json.Marshal("Use Read to read " + path + ". Then call stamp_result with the exact marker from the file. Finally report both file marker and stamp receipt. Do not invent tool results or use shell.")
	request := openAIRequest{Messages: []openAIMessage{{Role: "user", Content: prompt}}, Tools: []openAITool{tool, second}}
	model := "grok-4.7-xhigh-fast"
	var receipt string
	seenRead, seenStamp := false, false
	defer agentCheckpoints.reset()
	for turn := 0; turn < 6; turn++ {
		body, _ := json.Marshal(request)
		prepared, status := prepareChat(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "synthetic-live", Model: model, Payload: body, StorageJSON: []byte(credential)}})
		if status != nil {
			t.Fatal(status.Error())
		}
		if !resume {
			agentCheckpoints.reset()
		}
		agg := newCompletionAggregator(model)
		emitter := newChunkEmitter(model, false, false, agg.consume)
		emitter.stats = prepared.stats
		started := time.Now()
		if err := runAgentToolBridge(prepared, emitter); err != nil {
			t.Fatal(err.Error())
		}
		if err := emitter.finishChunk(prepared.parsed.InputChars); err != nil {
			t.Fatal(err)
		}
		response, err := agg.completion()
		if err != nil {
			t.Fatal(err)
		}
		var completion struct {
			Choices []struct {
				Message openAIMessage `json:"message"`
				Reason  string        `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(response, &completion); err != nil || len(completion.Choices) != 1 {
			t.Fatalf("bad completion %s", response)
		}
		assistant := completion.Choices[0].Message
		mode, _ := prepared.stats.summary()
		t.Logf("turn=%d mode=%s elapsed=%v finish=%s tools=%d tokens=[%s] text=%q", turn, mode, time.Since(started).Round(time.Millisecond), completion.Choices[0].Reason, len(assistant.ToolCalls), prepared.stats.tokenSummary(), contentText(assistant.Content))
		// summary 在模式后附带 " cp=saved|none"，只比较模式本身。
		if resume && turn > 0 && strings.SplitN(mode, " ", 2)[0] != "checkpoint" {
			t.Errorf("turn %d did not continue from the checkpoint: %s", turn, mode)
		}
		if len(assistant.ToolCalls) == 0 {
			answer := contentText(assistant.Content)
			if !seenRead || !seenStamp || !strings.Contains(answer, marker) || !strings.Contains(answer, receipt) {
				t.Fatalf("actual tool results not used: %q", answer)
			}
			return
		}
		request.Messages = append(request.Messages, assistant)
		for _, tc := range assistant.ToolCalls {
			var args map[string]string
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
				t.Fatal("invalid args")
			}
			var result string
			switch tc.Function.Name {
			case "Read":
				if args["file_path"] != path {
					t.Fatal("attempted access outside fixture")
				}
				bytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				result = string(bytes)
				seenRead = true
			case "stamp_result":
				if !seenRead || args["marker"] != marker {
					t.Fatal("second tool did not use actual first result")
				}
				rand.Read(nonce[:])
				receipt = "receipt_" + hex.EncodeToString(nonce[:])
				result = receipt
				seenStamp = true
			default:
				t.Fatalf("undeclared tool requested: %s", tc.Function.Name)
			}
			output, _ := json.Marshal(result)
			request.Messages = append(request.Messages, openAIMessage{Role: "tool", ToolCallID: tc.ID, Content: output})
		}
	}
	t.Fatal("tool loop did not complete within six turns")
}

// describeAgentPayload 给出 AgentServerMessage 的两层字段签名，不打印内容。
func describeAgentPayload(payload []byte) string {
	var out strings.Builder
	for _, f := range decodeMessage(payload) {
		fmt.Fprintf(&out, "f%d:%dB", f.Number, len(f.Value))
		if f.IsLen {
			inner := make([]string, 0, 4)
			for _, g := range decodeMessage(f.Value) {
				inner = append(inner, fmt.Sprintf("%d", g.Number))
			}
			fmt.Fprintf(&out, "[%s]", strings.Join(inner, ","))
		}
		out.WriteString(" ")
	}
	return out.String()
}
