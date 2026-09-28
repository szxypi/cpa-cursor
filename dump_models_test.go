package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDumpUsableModels(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("HOME") + "/.config/cursor/auth.json")
	if err != nil {
		t.Skip(err)
	}
	var auth struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatal(err)
	}
	headers := buildCursorHeaders(cursorIdentity{AccessToken: auth.AccessToken, GhostMode: true})
	headers["Content-Type"] = "application/proto"
	headers["User-Agent"] = "connectrpc/1.x-go"
	delete(headers, "Connect-Accept-Encoding")
	delete(headers, "Connect-Protocol-Version")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, err := fetchUsableModels(ctx, cursorAgentBase+cursorModelsPath, headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("body %d bytes", len(body))
	dumpProtoTree(body, 0)
}

// dumpProtoTree prints field numbers/wire types/values recursively.
func dumpProtoTree(data []byte, depth int) {
	fields := decodeMessage(data)
	for _, f := range fields {
		pad := ""
		for i := 0; i < depth; i++ {
			pad += "  "
		}
		if f.IsLen {
			if len(f.Value) > 0 && f.Value[0] < 0x80 && len(f.Value) < 64 && isPrintable(f.Value) {
				tlogf("%sfield %d LEN str %q", pad, f.Number, string(f.Value))
			} else {
				tlogf("%sfield %d LEN %d bytes", pad, f.Number, len(f.Value))
				dumpProtoTree(f.Value, depth+1)
			}
		} else {
			tlogf("%sfield %d VARINT %d", pad, f.Number, f.Varint)
		}
	}
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func tlogf(format string, args ...any) {
	println(fmt.Sprintf(format, args...))
}
