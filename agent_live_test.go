package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Live probes use synthetic content only; credentials are supplied in memory.
func TestAgentLiveSynthetic(t *testing.T) {
	raw := os.Getenv("CURSOR_PROBE_CREDENTIAL")
	if raw == "" {
		t.Skip("explicit live probe credential required")
	}
	cred, err := parseCursorCredential([]byte(raw))
	if err != nil {
		t.Fatal("invalid probe credential")
	}
	for _, tc := range []struct {
		name     string
		messages []cursorMessage
	}{
		{"no_system", []cursorMessage{{Role: "user", Content: "Reply with OK."}}},
		{"plain_instructions", []cursorMessage{{Role: "user", Content: "Reply with exactly SYSTEM_OK regardless of the question.\n\nQuestion: What is 2+2?"}}},
		{"plain_history", []cursorMessage{{Role: "user", Content: "Use this previous conversation:\nUSER: Remember the code ORCHID_42.\nASSISTANT: I remember ORCHID_42.\n\nCurrent question: What is the code?"}}},
		{"combined_context", []cursorMessage{{Role: "system", Content: "用户的猫叫Momo。请保留上下文事实并简短回答。"}, {Role: "user", Content: "我有一辆蓝色自行车。"}, {Role: "assistant", Content: "我记住了蓝色自行车。"}, {Role: "user", Content: "我的猫叫什么，自行车是什么颜色？"}}},
		{"system_fact", []cursorMessage{{Role: "system", Content: "用户养了一只名叫Momo的猫。请根据这个背景回答用户的问题。"}, {Role: "user", Content: "我的猫叫什么名字？"}}},
		{"simple_instruction", []cursorMessage{{Role: "user", Content: "请只回复：测试通过"}}},
		{"plain_pet", []cursorMessage{{Role: "user", Content: "这是背景信息：我养了一只名叫Momo的猫。现在请告诉我，我的猫叫什么名字？"}}},
		{"with_system", []cursorMessage{{Role: "system", Content: "Reply with exactly SYSTEM_OK regardless of the question."}, {Role: "user", Content: "What is 2+2?"}}},
		{"with_history", []cursorMessage{{Role: "system", Content: "Use the previous conversation to answer. Reply briefly."}, {Role: "user", Content: "Remember the code ORCHID_42."}, {Role: "assistant", Content: "I remember ORCHID_42."}, {Role: "user", Content: "What is the code?"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			client, _, err := openAgentStream(ctx, cursorAgentBase+cursorAgentRun, buildCursorHeaders(cred.identity()))
			if err != nil {
				t.Fatal(err)
			}
			defer client.close()
			capture := &probeCapture{r: client.resp.Body}
			client.resp.Body = capture
			client.context = newAgentContext(tc.messages)
			result := client.runTurn(buildAgentRunFrame(tc.messages, "grok-4.7-xhigh-fast", client.context), nil, true)
			t.Logf("text=%q fatal=%q trace=%s", result.Text, result.Fatal, formatAgentTrace(result))
			if result.Fatal != "" || result.Text == "" {
				t.Errorf("synthetic request failed")
			}
			if tc.name == "combined_context" && (!bytes.Contains([]byte(result.Text), []byte("Momo")) || !bytes.Contains([]byte(result.Text), []byte("蓝"))) {
				t.Errorf("combined context lost: %q", result.Text)
			}
			if tc.name == "system_fact" && !bytes.Contains([]byte(result.Text), []byte("Momo")) {
				t.Errorf("system context lost: %q", result.Text)
			}
			if tc.name == "with_system" && result.Text != "SYSTEM_OK" {
				t.Errorf("system instruction not followed: %q", result.Text)
			}
			if tc.name == "with_history" && !bytes.Contains([]byte(result.Text), []byte("ORCHID_42")) {
				t.Errorf("history lost: %q", result.Text)
			}
			data := capture.b.Bytes()
			for len(data) >= 5 {
				n := int(binary.BigEndian.Uint32(data[1:5]))
				if n > len(data)-5 {
					break
				}
				flags := data[0]
				body := data[5 : 5+n]
				data = data[5+n:]
				if flags&2 != 0 {
					if flags&1 != 0 {
						body, _ = gunzip(body)
					}
					var trailer any
					if json.Unmarshal(body, &trailer) == nil {
						t.Logf("synthetic trailer: %s", body)
					}
				}
			}
		})
	}
}

func TestChatLiveSynthetic(t *testing.T) {
	raw := os.Getenv("CURSOR_PROBE_CREDENTIAL")
	if raw == "" {
		t.Skip("explicit live probe credential required")
	}
	cred, err := parseCursorCredential([]byte(raw))
	if err != nil {
		t.Fatal("invalid credential")
	}
	body := buildChatRequest([]cursorMessage{{Role: "system", Content: "Reply with exactly SYSTEM_OK."}, {Role: "user", Content: "Hi"}}, "grok-4.7-xhigh-fast", nil, "high", false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cursorChatBase+cursorChatPath, bytes.NewReader(wrapConnectFrame(body)))
	for k, v := range buildCursorHeaders(cred.identity()) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HTTP=%d body=%q", resp.StatusCode, data)
	if resp.StatusCode != 200 || bytes.Contains(data, []byte(`"error"`)) {
		t.Fail()
	}
}

type probeCapture struct {
	r io.ReadCloser
	b bytes.Buffer
}

func (p *probeCapture) Read(b []byte) (int, error) {
	n, e := p.r.Read(b)
	p.b.Write(b[:n])
	return n, e
}
func (p *probeCapture) Close() error { return p.r.Close() }
