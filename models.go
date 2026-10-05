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
	{ID: "auto", Name: "auto", DisplayName: "Auto", Description: "Auto"},
	{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex", DisplayName: "Codex 5.3", Description: "Codex 5.3"},
	{ID: "gpt-5.3-codex-fast", Name: "gpt-5.3-codex-fast", DisplayName: "Codex 5.3 Fast", Description: "Codex 5.3 Fast"},
	{ID: "gpt-5.2", Name: "gpt-5.2", DisplayName: "GPT-5.2", Description: "GPT-5.2"},
	{ID: "composer-2.5", Name: "composer-2.5", DisplayName: "Composer 2.5", Description: "Composer 2.5"},
	{ID: "claude-opus-5-thinking-high", Name: "claude-opus-5-thinking-high", DisplayName: "Claude Opus 5 1M Thinking", Description: "Claude Opus 5 1M Thinking"},
	{ID: "claude-opus-5-thinking-high-fast", Name: "claude-opus-5-thinking-high-fast", DisplayName: "Claude Opus 5 1M Thinking Fast", Description: "Claude Opus 5 1M Thinking Fast"},
	{ID: "gpt-5.6-sol-high", Name: "gpt-5.6-sol-high", DisplayName: "GPT-5.6 Sol 1M High", Description: "GPT-5.6 Sol 1M High"},
	{ID: "gpt-5.6-sol-high-fast", Name: "gpt-5.6-sol-high-fast", DisplayName: "GPT-5.6 Sol 1M High Fast", Description: "GPT-5.6 Sol 1M High Fast"},
	{ID: "claude-fable-5-thinking-high", Name: "claude-fable-5-thinking-high", DisplayName: "Claude Fable 5 1M Thinking (NO ZDR)", Description: "Claude Fable 5 1M Thinking (NO ZDR)"},
	{ID: "cursor-grok-4.5-high", Name: "cursor-grok-4.5-high", DisplayName: "Grok 4.5", Description: "Grok 4.5"},
	{ID: "cursor-grok-4.5-high-fast", Name: "cursor-grok-4.5-high-fast", DisplayName: "Grok 4.5 Fast", Description: "Grok 4.5 Fast"},
	{ID: "gemini-3.7-flash-high", Name: "gemini-3.7-flash-high", DisplayName: "Gemini 3.7 Flash", Description: "Gemini 3.7 Flash"},
	{ID: "claude-sonnet-5-thinking-high", Name: "claude-sonnet-5-thinking-high", DisplayName: "Claude Sonnet 5 1M Thinking", Description: "Claude Sonnet 5 1M Thinking"},
	{ID: "gpt-5.6-luna-high", Name: "gpt-5.6-luna-high", DisplayName: "GPT-5.6 Luna 1M High", Description: "GPT-5.6 Luna 1M High"},
	{ID: "grok-4.7-high", Name: "grok-4.7-high", DisplayName: "Grok 4.7  High", Description: "Grok 4.7  High"},
	{ID: "grok-4.7-high-fast", Name: "grok-4.7-high-fast", DisplayName: "", Description: "grok-4.7-high-fast"},
	{ID: "cursor-grok-4.6-high", Name: "cursor-grok-4.6-high", DisplayName: "Grok 4.6", Description: "Grok 4.6"},
	{ID: "cursor-grok-4.6-high-fast", Name: "cursor-grok-4.6-high-fast", DisplayName: "Grok 4.6 Fast", Description: "Grok 4.6 Fast"},
	{ID: "composer-2.5-fast", Name: "composer-2.5-fast", DisplayName: "Composer 2.5 Fast", Description: "Composer 2.5 Fast"},
	{ID: "claude-opus-5-5-medium", Name: "claude-opus-5-5-medium", DisplayName: "Claude Opus 5.5 1M", Description: "Claude Opus 5.5 1M"},
	{ID: "claude-opus-5-5-medium-fast", Name: "claude-opus-5-5-medium-fast", DisplayName: "Claude Opus 5.5 1M Fast", Description: "Claude Opus 5.5 1M Fast"},
	{ID: "claude-opus-5-5-high", Name: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 1M High", Description: "Claude Opus 5.5 1M High"},
	{ID: "claude-opus-5-5-high-fast", Name: "claude-opus-5-5-high-fast", DisplayName: "Claude Opus 5.5 1M High Fast", Description: "Claude Opus 5.5 1M High Fast"},
	{ID: "gpt-5.2-fast", Name: "gpt-5.2-fast", DisplayName: "GPT-5.2 Fast", Description: "GPT-5.2 Fast"},
	{ID: "gpt-5.6-luna-high-fast", Name: "gpt-5.6-luna-high-fast", DisplayName: "GPT-5.6 Luna 1M High Fast", Description: "GPT-5.6 Luna 1M High Fast"},
	{ID: "kimi-k3-max", Name: "kimi-k3-max", DisplayName: "Kimi K3", Description: "Kimi K3"},
	{ID: "glm-5.2-max", Name: "glm-5.2-max", DisplayName: "GLM 5.2 Max", Description: "GLM 5.2 Max"},
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
	ID      string
	Name    string
	Aliases []string
}

// parseUsableModels decodes the GetUsableModels protobuf as observed on
// 2026-09-28: repeated field 1, each entry {slug: 1, id: 3, name: 4, aliases: 6*}.
// (9router's older shape — nested list + name at 2 — no longer matches the
// live response and yields empty ids.)
func parseUsableModels(data []byte) []cursorUsableModel {
	var out []cursorUsableModel
	for _, field := range decodeMessage(data) {
		if field.Number != 1 || !field.IsLen {
			continue
		}
		entry := decodeMessage(field.Value)
		model := cursorUsableModel{}
		if f, ok := fieldFirst(entry, 3); ok && f.IsLen {
			model.ID = string(f.Value)
		}
		if f, ok := fieldFirst(entry, 4); ok && f.IsLen {
			model.Name = string(f.Value)
		}
		for _, f := range entry {
			if f.Number == 6 && f.IsLen {
				model.Aliases = append(model.Aliases, string(f.Value))
			}
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

	models := filterByPolicy(append([]pluginapi.ModelInfo(nil), staticCursorModels...))
	account := fetchLiveCatalog(identity)
	if len(account) > 0 {
		// Families come from the whole account catalog so that the panel
		// selection decides which families are listed, not which efforts a
		// listed family can reach.
		storeCursorFamilies(key, deriveCursorFamilies(account))
	}
	live := filterByPolicy(account)
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
	models = append(models, filterByPolicy(cursorFamilyInfos(cachedCursorFamilies(key)))...)

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
