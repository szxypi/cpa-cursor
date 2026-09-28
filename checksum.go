package main

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cursor client identity headers, ported from 9router's
// open-sse/utils/cursorChecksum.js (buildCursorHeaders). The pinned client
// build mirrors Cursor desktop 3.12.17 as observed there.
const (
	cursorClientVersion = "3.12.17"
	cursorClientCommit  = "0fb762053c34788bb7760d5673f8a6d4c8589d50"

	cursorChatBase   = "https://api2.cursor.sh"
	cursorChatPath   = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
	cursorAgentBase  = "https://agent.api5.cursor.sh"
	cursorAgentRun   = "/agent.v1.AgentService/Run"
	cursorModelsPath = "/agent.v1.AgentService/GetUsableModels"

	cursorUserAgent = "connect-es/1.6.1"
)

// cursorAlphabet is the URL-safe base64 variant the checksum uses (last two
// characters swapped versus RFC 4648, no padding).
const cursorAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// cursorIdentity is the per-credential material the headers derive from.
// MachineID is optional: Cursor derives a stable one from the token when the
// import source did not capture it.
type cursorIdentity struct {
	AccessToken string
	MachineID   string
	GhostMode   bool
}

// cleanToken mirrors buildCursorHeaders: a token captured as "prefix::token"
// keeps only the part after the separator.
func cleanToken(token string) string {
	if idx := strings.Index(token, "::"); idx >= 0 {
		return token[idx+2:]
	}
	return token
}

// generateHashed64Hex mirrors generateHashed64Hex (sha256 of input+salt).
func generateHashed64Hex(input, salt string) string {
	sum := sha256.Sum256([]byte(input + salt))
	return fmt.Sprintf("%x", sum)
}

// resolvedMachineID: a stored machineId wins, otherwise one is derived
// deterministically from the token so replayed checksums stay consistent.
func (id cursorIdentity) resolvedMachineID() string {
	if id.MachineID != "" {
		return id.MachineID
	}
	return generateHashed64Hex(cleanToken(id.AccessToken), "machineId")
}

// obfuscateChecksumBytes is the "Jyh cipher" loop: rolling XOR with the
// previous output byte plus the index, all mod 256.
func obfuscateChecksumBytes(b []byte) {
	t := byte(165)
	for i := 0; i < len(b); i++ {
		final := (b[i] ^ t) + byte(i%256)
		b[i] = final
		t = final
	}
}

// generateCursorChecksum builds the x-cursor-checksum value: the 6-byte
// big-endian timestamp (floor(ms/1e6)) run through the obfuscation loop,
// encoded with Cursor's custom base64 alphabet without padding, with the
// machine id appended verbatim.
func generateCursorChecksum(machineID string) string {
	ts := time.Now().UnixMilli() / 1_000_000
	buf := []byte{
		byte(ts >> 40), byte(ts >> 32), byte(ts >> 24),
		byte(ts >> 16), byte(ts >> 8), byte(ts),
	}
	obfuscateChecksumBytes(buf)

	var encoded strings.Builder
	for i := 0; i < len(buf); i += 3 {
		a := buf[i]
		b := byte(0)
		c := byte(0)
		if i+1 < len(buf) {
			b = buf[i+1]
		}
		if i+2 < len(buf) {
			c = buf[i+2]
		}
		encoded.WriteByte(cursorAlphabet[a>>2])
		encoded.WriteByte(cursorAlphabet[((a&3)<<4)|(b>>4)])
		if i+1 < len(buf) {
			encoded.WriteByte(cursorAlphabet[((b&15)<<2)|(c>>6)])
		}
		if i+2 < len(buf) {
			encoded.WriteByte(cursorAlphabet[c&63])
		}
	}
	return encoded.String() + machineID
}

// clientOS reports the platform tag Cursor's client headers carry. The plugin
// always presents the host it runs on.
func clientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func clientArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

func localTimezone() string {
	name := time.Local.String()
	if name == "" || name == "Local" {
		return "UTC"
	}
	return name
}

// Environment overrides let operators pin a newer Cursor client build without
// rebuilding the plugin (env on the cli-proxy-api unit).
var (
	cursorClientVersionOverride = os.Getenv("CURSOR_CLIENT_VERSION")
	cursorClientCommitOverride  = os.Getenv("CURSOR_CLIENT_COMMIT")
)

func effectiveClientVersion() string {
	if cursorClientVersionOverride != "" {
		return cursorClientVersionOverride
	}
	return cursorClientVersion
}

func effectiveClientCommit() string {
	if cursorClientCommitOverride != "" {
		return cursorClientCommitOverride
	}
	return cursorClientCommit
}

// buildCursorHeaders assembles the header set every Cursor upstream call
// needs. The endpoint is not part of the checksum, so one builder serves both
// the ChatService and the AgentService.
func buildCursorHeaders(id cursorIdentity) map[string]string {
	token := cleanToken(id.AccessToken)
	machineID := id.resolvedMachineID()
	ghost := "false"
	if id.GhostMode {
		ghost = "true"
	}
	timezone := localTimezone()
	return map[string]string{
		"Authorization":               "Bearer " + token,
		"Connect-Accept-Encoding":     "gzip",
		"Connect-Protocol-Version":    "1",
		"Content-Type":                "application/connect+proto",
		"User-Agent":                  cursorUserAgent,
		"x-amzn-trace-id":             "Root=" + randomUUID(),
		"x-client-key":                generateHashed64Hex(token, ""),
		"x-cursor-checksum":           generateCursorChecksum(machineID),
		"x-cursor-client-version":     effectiveClientVersion(),
		"x-cursor-client-commit":      effectiveClientCommit(),
		"x-cursor-client-type":        "ide",
		"x-cursor-client-os":          clientOS(),
		"x-cursor-client-arch":        clientArch(),
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":     randomUUID(),
		"x-cursor-timezone":           timezone,
		"x-ghost-mode":                ghost,
		"x-request-id":                randomUUID(),
		"x-session-id":                generateSessionID(token),
	}
}

// generateSessionID is uuid v5 in the DNS namespace over the token.
func generateSessionID(token string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(token)).String()
}

func randomUUID() string {
	id, err := uuid.NewRandomFromReader(rand.Reader)
	if err != nil {
		return uuid.New().String()
	}
	return id.String()
}
