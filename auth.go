package main

// Auth provider methods. CPA drives these from three places:
//
//	auth.parse   - the auth-directory watcher, for every file it does not
//	               recognize itself (hand-placed cursor-*.json)
//	auth.models  - the model discoverer, once per credential at startup and
//	               after config reloads
//	auth.refresh - the credential refresher

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if !looksLikeCursorCredential(req.RawJSON) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	cred, err := parseCursorCredential(req.RawJSON)
	if err != nil {
		// The file claims to be ours but is unusable; say so instead of
		// leaving it to be reported as an unknown provider.
		return nil, fmt.Errorf("parse cursor credential %s: %w", req.FileName, err)
	}
	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" {
		fileName = cred.fileName()
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    providerKey,
			FileName:    fileName,
			Label:       cred.label(),
			StorageJSON: cred.storageJSON(),
			Metadata:    cred.metadata(),
		},
	})
}

// handleAuthModels merges the account's live GetUsableModels catalog over the
// static list (10-minute cache per token, mirroring 9router's catalog cache).
func handleAuthModels(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	cred, err := parseCursorCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	models := catalogForToken(cred.identity())
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   models,
	})
}

// handleAuthRefresh is a no-op by design: imported IDE tokens have no
// server-side refresh flow (9router's refreshCredentials returns null), so
// the credential is used until Cursor rotates it client-side. Returning it
// unchanged with a far NextRefreshAfter keeps CPA's refresher quiet.
func handleAuthRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	cred, err := parseCursorCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    providerKey,
			ID:          req.AuthID,
			Label:       cred.label(),
			StorageJSON: req.StorageJSON,
			Metadata:    req.Metadata,
			Attributes:  req.Attributes,
		},
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	})
}

// metadata returns the mutable host-managed fields CPA surfaces.
func (c *cursorCredential) metadata() map[string]any {
	meta := map[string]any{
		"AuthMethod": "import",
	}
	if c.Email != "" {
		meta["Email"] = c.Email
	}
	if c.GhostMode != nil {
		meta["GhostMode"] = *c.GhostMode
	}
	return meta
}

// fileName generates the deterministic auth file name for imported tokens.
func (c *cursorCredential) fileName() string {
	return "cursor-" + shortTokenHash(cleanToken(c.AccessToken)) + ".json"
}
