package main

// Response-side decoding tests with synthetic frames: text deltas, thinking,
// tool calls, JSON error frames, gzip compression, and chunk reassembly.

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"strings"
	"testing"
)

func gz(data []byte) []byte {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, _ = writer.Write(data)
	_ = writer.Close()
	return buf.Bytes()
}

// synthTextFrame builds a ChatService response sub-message {text: s}.
func synthTextFrame(s string) []byte {
	return wrapConnectFrame(fieldBytes(respResponse, fieldString(respText, s)))
}

func synthThinkingFrame(s string) []byte {
	return wrapConnectFrame(fieldBytes(respResponse, fieldBytes(respThinking, fieldString(thinkingText, s))))
}

func synthToolFrame(id, args string, isLast bool) []byte {
	mcp := fieldBytes(mcpToolsList, concat(
		fieldString(mcpNestedName, "read_file"),
		fieldString(mcpNestedParams, args),
	))
	call := concat(
		fieldString(toolID, id),
		fieldString(toolName, "read_file"),
		fieldString(toolRawArgs, args),
		fieldVarint(toolIsLast, boolToVarint(isLast)),
		fieldBytes(toolMcpParam, mcp),
	)
	return wrapConnectFrame(fieldBytes(respToolCall, call))
}

func synthErrorFrame(json string) []byte {
	return wrapConnectFrame([]byte(json))
}

func TestFrameDecoderTextAndThinking(t *testing.T) {
	decoder := newFrameDecoder()
	texts, _, _, fatal := decoder.feed(synthTextFrame("he"))
	moreTexts, thinks, tools, fatal2 := decoder.feed(synthTextFrame("llo"))
	texts = append(texts, moreTexts...)
	fatal = fatal2
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	if len(texts) != 2 || texts[0] != "he" || texts[1] != "llo" {
		t.Fatalf("texts = %q", texts)
	}
	if len(thinks) != 0 || len(tools) != 0 {
		t.Fatalf("unexpected extra events")
	}
	_, thinks, _, _ = newFrameDecoder().feed(synthThinkingFrame("hmm"))
	if len(thinks) != 1 || thinks[0] != "hmm" {
		t.Fatalf("thinks = %q", thinks)
	}
}

func TestFrameDecoderAcrossChunkBoundaries(t *testing.T) {
	a := synthTextFrame("abc")
	b := synthTextFrame("def")
	stream := append(append([]byte{}, a...), b...)
	split := len(a) + 5 + 1
	decoder := newFrameDecoder()
	texts, _, _, fatal := decoder.feed(stream[:split])
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	if len(texts) != 1 {
		t.Fatalf("texts after first chunk = %q", texts)
	}
	texts, _, _, fatal = decoder.feed(stream[split:])
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	if len(texts) != 1 || texts[0] != "def" {
		t.Fatalf("texts after second chunk = %q", texts)
	}
}

func TestFrameDecoderGzip(t *testing.T) {
	payload := fieldBytes(respResponse, fieldString(respText, "zipped"))
	zipped := gz(payload)
	frame := []byte{0x01, byte(len(zipped) >> 24), byte(len(zipped) >> 16), byte(len(zipped) >> 8), byte(len(zipped))}
	frame = append(frame, zipped...)
	texts, _, _, fatal := newFrameDecoder().feed(frame)
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	if len(texts) != 1 || texts[0] != "zipped" {
		t.Fatalf("texts = %q", texts)
	}
}

func TestFrameDecoderToolCall(t *testing.T) {
	decoder := newFrameDecoder()
	_, _, tools, fatal := decoder.feed(synthToolFrame("call-1", `{"path":"x"}`, false))
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	_, _, moreTools, fatal := decoder.feed(synthToolFrame("call-1", `{"path":"x"}`, true))
	tools = append(tools, moreTools...)
	if fatal != nil {
		t.Fatalf("fatal: %v", fatal)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].ID != "call-1" || tools[0].Name != "read_file" {
		t.Fatalf("first tool = %+v", tools[0])
	}
	if tools[1].ID != "call-1" || !tools[1].IsLast {
		t.Fatalf("last tool = %+v", tools[1])
	}
}

func TestFrameDecoderErrorFrame(t *testing.T) {
	_, _, _, fatal := newFrameDecoder().feed(synthErrorFrame(`{"error":{"code":"resource_exhausted","message":"quota"}}`))
	if fatal == nil || !strings.Contains(fatal.Error(), "quota") {
		t.Fatalf("fatal = %v", fatal)
	}
}

func TestExtractChatResultHexVector(t *testing.T) {
	payload, _ := hex.DecodeString("12050a03616263")
	result := extractChatResult(payload)
	if result.Text != "abc" {
		t.Fatalf("text = %q", result.Text)
	}
}
