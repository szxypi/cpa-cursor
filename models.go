package main

// Model catalog: a static fallback mirroring 9router's registry list plus a
// live fetch of GetUsableModels per account (auth.models), cached briefly.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// staticCursorModels mirrors 9router's providerModels cursor row. Context and
// thinking metadata follows what the catalog there declares.
var staticCursorModels = []pluginapi.ModelInfo{
	{ID: "auto", Name: "auto", Description: "Cursor Auto (pro / max only)"},
	{ID: "claude-4.5-sonnet", Name: "claude-4.5-sonnet", Description: "Anthropic Claude 4.5 Sonnet (cursor-pro / max)"},
	{ID: "claude-4.5-sonnet-thinking", Name: "claude-4.5-sonnet-thinking", Description: "Anthropic Claude 4.5 Sonnet with thinking (cursor-pro / max)"},
	{ID: "claude-4.5-sonnet-max", Name: "claude-4.5-sonnet-max", Description: "Anthropic Claude 4.5 Sonnet Max (cursor-max)"},
	{ID: "gpt-5.2", Name: "gpt-5.2", Description: "OpenAI GPT-5.2 (cursor-pro / max)"},
	{ID: "gpt-5.2-high", Name: "gpt-5.2-high", Description: "OpenAI GPT-5.2 with high reasoning (cursor-pro / max)"},
	{ID: "gpt-5.2-thinking", Name: "gpt-5.2-thinking", Description: "OpenAI GPT-5.2 Thinking (cursor-pro / max)"},
	{ID: "gpt-5.2-codex", Name: "gpt-5.2-codex", Description: "OpenAI GPT-5.2 Codex (cursor-pro / max)"},
	{ID: "gpt-5.2-codex-thinking", Name: "gpt-5.2-codex-thinking", Description: "OpenAI GPT-5.2 Codex Thinking (cursor-pro / max)"},
	{ID: "gemini-3.8-pro", Name: "gemini-3.8-pro", Description: "Google Gemini 3.8 Pro (cursor-pro / max)"},
	{ID: "gemini-3.8-flash", Name: "gemini-3.8-flash", Description: "Google Gemini 3.8 Flash (cursor-pro / max)"},
	{ID: "composer-1", Name: "composer-1", Description: "Cursor Composer 1 (all plans)"},
	{ID: "composer-1-thinking", Name: "composer-1-thinking", Description: "Cursor Composer 1 Thinking (all plans)"},
	{ID: "auto-gpt-5.2", Name: "auto-gpt-5.2", Description: "Cursor Auto GPT-5.2 (cursor-pro / max)"},
}

// applyCursorModelTiers fills the tier metadata CPA displays; done in code so
// the static list above stays declarative.
func applyCursorModelTiers(models []pluginapi.ModelInfo) {
	for i := range models {
		id := models[i].ID
		models[i].Type = "chat"
		models[i].Object = "model"
		models[i].InputTokenLimit = 200000
		models[i].OutputTokenLimit = 16384
		models[i].ContextLength = 200000
		models[i].MaxCompletionTokens = 65536
		if strings.Contains(id, "thinking") {
			// reasoning_effort medium/high map to Cursor's thinking_level
			models[i].Thinking = &pluginapi.ThinkingSupport{
				Min:            0,
				Max:            2,
				ZeroAllowed:    true,
				DynamicAllowed: true,
				Levels:         []string{"medium", "high"},
			}
		}
	}
}

// cursorModelCatalog caches the per-token GetUsableModels result.
var (
	cursorCatalogMu    sync.Mutex
	cursorCatalogCache = map[string]cursorCatalogEntry{}
)

type cursorCatalogEntry struct {
	models []pluginapi.ModelInfo
	at     time.Time
}

const cursorCatalogTTL = 10 * time.Minute

// cursorUsableModel is one entry of the GetUsableModels response.
type cursorUsableModel struct {
	ID   string
	Name string
}

// parseUsableModels decodes the GetUsableModels protobuf: repeated field 1,
// each entry {id: 1 string, name: 2 string, features: 3 message}. Mirrors
// parseUsableModelsResponse with the official-id filtering removed (this
// plugin trusts the account's own list).
func parseUsableModels(data []byte) []cursorUsableModel {
	var out []cursorUsableModel
	for _, field := range decodeMessage(data) {
		if field.Number != 1 || !field.IsLen {
			continue
		}
		entry := decodeMessage(field.Value)
		model := cursorUsableModel{}
		if f, ok := fieldFirst(entry, 1); ok && f.IsLen {
			model.ID = string(f.Value)
		}
		if f, ok := fieldFirst(entry, 2); ok && f.IsLen {
			model.Name = string(f.Value)
		}
		if model.ID != "" {
			out = append(out, model)
		}
	}
	return out
}

// catalogForToken merges the account's live catalog over the static list.
func catalogForToken(identity cursorIdentity) []pluginapi.ModelInfo {
	key := cleanToken(identity.AccessToken)
	cursorCatalogMu.Lock()
	entry, cached := cursorCatalogCache[key]
	cursorCatalogMu.Unlock()
	if cached && time.Since(entry.at) < cursorCatalogTTL {
		return entry.models
	}

	models := append([]pluginapi.ModelInfo(nil), staticCursorModels...)
	live := fetchLiveCatalog(identity)
	if len(live) > 0 {
		byID := map[string]pluginapi.ModelInfo{}
		for _, m := range live {
			byID[m.ID] = m
		}
		// static first (stable order, carries tier metadata), then anything
		// the account exposes that the static list missed.
		for i := range models {
			if live, ok := byID[models[i].ID]; ok && live.Description != "" {
				models[i].Description = live.Description
				delete(byID, models[i].ID)
			}
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			models = append(models, byID[id])
		}
	}
	applyCursorModelTiers(models)

	cursorCatalogMu.Lock()
	cursorCatalogCache[key] = cursorCatalogEntry{models: models, at: time.Now()}
	cursorCatalogMu.Unlock()
	return models
}

func fetchLiveCatalog(identity cursorIdentity) []pluginapi.ModelInfo {
	headers := buildCursorHeaders(identity)
	headers["Content-Type"] = "application/proto"
	headers["User-Agent"] = "connectrpc/1.x-go"
	delete(headers, "Connect-Accept-Encoding")
	delete(headers, "Connect-Protocol-Version")

	ctx, cancel := contextWithTimeout(8 * time.Second)
	defer cancel()
	body, err := fetchUsableModels(ctx, cursorAgentBase+cursorModelsPath, headers)
	if err != nil {
		hostLog("warn", fmt.Sprintf("cursor live catalog failed, falling back to static list: %v", err))
		return nil
	}
	var out []pluginapi.ModelInfo
	for _, m := range parseUsableModels(body) {
		info := pluginapi.ModelInfo{ID: m.ID, Name: m.ID, DisplayName: m.Name}
		out = append(out, info)
	}
	return out
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
