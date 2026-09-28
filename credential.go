package main

// Cursor credential file format. Cursor has no OAuth dance a server can run:
// the token is captured from a logged-in Cursor IDE (state.vscdb
// cursorAuth/accessToken) and imported verbatim, so the credential is just
// the token plus optional machine identity.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// shortTokenHash derives the stable suffix for generated auth file names.
func shortTokenHash(token string) string {
	sum := sha256.Sum256([]byte("cursor:" + token))
	return hex.EncodeToString(sum[:])[:8]
}

type cursorCredential struct {
	AccessToken string `json:"access_token"`
	MachineID   string `json:"machine_id,omitempty"`
	Email       string `json:"email,omitempty"`
	Label       string `json:"label,omitempty"`
	GhostMode   *bool  `json:"ghost_mode,omitempty"`
}

// parseCursorCredential decodes the storage JSON of a cursor auth file.
func parseCursorCredential(storage []byte) (*cursorCredential, error) {
	if len(strings.TrimSpace(string(storage))) == 0 {
		return nil, fmt.Errorf("empty credential storage")
	}
	var cred cursorCredential
	if err := json.Unmarshal(storage, &cred); err != nil {
		return nil, fmt.Errorf("decode cursor credential: %w", err)
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		// Tolerate the 9router / other-router layout that nests the fields.
		var nested struct {
			AccessToken string `json:"accessToken"`
			MachineID   string `json:"machineId"`
		}
		if err := json.Unmarshal(storage, &nested); err == nil && nested.AccessToken != "" {
			cred.AccessToken = nested.AccessToken
			cred.MachineID = nested.MachineID
		}
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return nil, fmt.Errorf("credential has no access_token")
	}
	return &cred, nil
}

// looksLikeCursorCredential reports whether a stray JSON file is a cursor
// credential worth claiming during auth.parse. Being strict here avoids
// stealing files that merely mention a token field.
func looksLikeCursorCredential(storage []byte) bool {
	cred, err := parseCursorCredential(storage)
	return err == nil && cred != nil
}

// identity converts the credential into the header-building identity.
func (c *cursorCredential) identity() cursorIdentity {
	ghost := true
	if c.GhostMode != nil {
		ghost = *c.GhostMode
	}
	return cursorIdentity{
		AccessToken: c.AccessToken,
		MachineID:   c.MachineID,
		GhostMode:   ghost,
	}
}

// storageJSON serializes the credential back for host.auth.save.
func (c *cursorCredential) storageJSON() []byte {
	storage := map[string]any{
		"type":         "cursor",
		"access_token": c.AccessToken,
	}
	if c.MachineID != "" {
		storage["machine_id"] = c.MachineID
	}
	if c.Email != "" {
		storage["email"] = c.Email
	}
	if c.GhostMode != nil {
		storage["ghost_mode"] = *c.GhostMode
	}
	raw, _ := json.Marshal(storage)
	return raw
}

// label produces the account label CPA surfaces in the management API.
func (c *cursorCredential) label() string {
	if c.Label != "" {
		return c.Label
	}
	if c.Email != "" {
		return c.Email
	}
	token := cleanToken(c.AccessToken)
	if len(token) > 12 {
		return "cursor-" + token[:8] + "…"
	}
	return "cursor-account"
}
