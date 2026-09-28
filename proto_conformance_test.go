package main

// Byte-level conformance against 9router's JS implementation: same fixture,
// same pinned uuid/time, same wire bytes. Regenerate the vector with
// `node scripts/conformance/run.mjs` (it copies 9router's encoder into a
// sandbox with a deterministic uuid mock and frozen clock).

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

func load9routerVector(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("want-9router.txt")
	if err != nil {
		t.Skip("want-9router.txt not present; run node scripts/conformance/run.mjs")
	}
	return strings.TrimSpace(string(raw))
}

// pinnedUUIDs mirrors the sandbox's deterministic uuid v4 sequence.
func pinnedUUIDs() (ids []string) {
	for i := 1; i <= 64; i++ {
		tail := strings.Repeat("0", 4-len(itoa(i))) + itoa(i)
		ids = append(ids, "00000000-0000-4000-8000-00000000"+tail)
	}
	return ids
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func conformanceFixture() ([]cursorMessage, []cursorTool) {
	messages := []cursorMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "and <tool_result>\n<tool_name>t</tool_name>\n</tool_result> done"},
	}
	tools := []cursorTool{
		{Name: "get_weather", Description: "Get weather", Arguments: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`},
		{Name: "mcp__server__read_file", Description: "Reads a file", Arguments: `{"type":"object","properties":{"path":{"type":"string"}}}`},
	}
	return messages, tools
}

func TestChatBodyMatches9router(t *testing.T) {
	messages, tools := conformanceFixture()

	ids := pinnedUUIDs()
	restoreUUID, restoreTime := uuidFactory, timeNowISO
	uuidFactory = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	timeNowISO = func() string { return "2026-09-28T12:00:00.000Z" }
	defer func() { uuidFactory, timeNowISO = restoreUUID, restoreTime }()

	body := buildChatRequest(messages, "claude-4.5-sonnet", tools, "medium", true)
	got := hex.EncodeToString(body)
	want := load9routerVector(t)
	if got != want {
		t.Fatalf("chat body mismatch:\n got  %s\n want %s", got, want)
	}
}

// TestAgentFrameMatches9router pins the AgentService run frame against
// scripts/conformance (crypto.randomUUID pinned to the same sequence).
func TestAgentFrameMatches9router(t *testing.T) {
	original := uuidFactory
	defer func() { uuidFactory = original }()
	seq := 0
	uuidFactory = func() string {
		seq++
		return fmt.Sprintf("10000000-0000-4000-8000-%012d", seq)
	}

	messages := []cursorMessage{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "what is 2+2"},
	}
	got := hex.EncodeToString(buildAgentRunFrame(messages, "composer-1"))

	wantBytes, err := os.ReadFile("want-9router-agent.txt")
	if err != nil {
		t.Skipf("run scripts/conformance/run.mjs first: %v", err)
	}
	want := strings.TrimSpace(string(wantBytes))
	if got != want {
		t.Fatalf("agent frame mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestAgentRunFrameRoundTrip(t *testing.T) {
	messages, _ := conformanceFixture()
	frame := buildAgentRunFrame(messages, "composer-1")
	// buildAgentRunFrame returns a complete connect frame already.
	payloadLen := int(frame[1])<<24 | int(frame[2])<<16 | int(frame[3])<<8 | int(frame[4])
	if frame[0] != 0 || payloadLen != len(frame)-5 {
		t.Fatalf("connect frame header wrong: % x", frame[:5])
	}
	// Uncompressed payloads pass through decompressFramePayload untouched.
	if !bytesEqual(decompressFramePayload(frame[5:], 0), frame[5:]) {
		t.Fatalf("passthrough failed")
	}
}

func TestChecksumShape(t *testing.T) {
	const machineID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sum := generateCursorChecksum(machineID)
	if !strings.HasSuffix(sum, machineID) {
		t.Fatalf("checksum must end with machineId: %q", sum)
	}
	encoded := strings.TrimSuffix(sum, machineID)
	// 6 timestamp bytes encode to 8 base64 characters.
	if len(encoded) != 8 {
		t.Fatalf("encoded part should be 8 chars: %q", encoded)
	}
	if strings.ContainsAny(encoded, "+/") {
		t.Fatalf("checksum contains standard base64 chars: %q", encoded)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
