package main

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func catalogOf(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id})
	}
	return out
}

func TestSplitCursorVariant(t *testing.T) {
	cases := []struct {
		id, base, effort string
		fast, ok         bool
	}{
		{"grok-4.7-xhigh-fast", "grok-4.7", "xhigh", true, true},
		{"grok-4.7-low", "grok-4.7", "low", false, true},
		{"gpt-5.6-luna-none", "gpt-5.6-luna", "none", false, true},
		{"claude-opus-5-thinking-high-fast", "claude-opus-5-thinking", "high", true, true},
		{"cursor-grok-4.6-xhigh-fast", "cursor-grok-4.6", "xhigh", true, true},
		{"composer-2.5-fast", "", "", false, false},
		{"gpt-5.3-codex-fast", "", "", false, false},
		{"claude-4.6-opus-high-thinking", "", "", false, false},
		{"auto", "", "", false, false},
		{"high", "", "", false, false},
	}
	for _, c := range cases {
		base, effort, fast, ok := splitCursorVariant(c.id)
		if ok != c.ok || (ok && (base != c.base || effort != c.effort || fast != c.fast)) {
			t.Errorf("splitCursorVariant(%q) = %q %q %v %v, want %q %q %v %v",
				c.id, base, effort, fast, ok, c.base, c.effort, c.fast, c.ok)
		}
	}
}

func TestDeriveCursorFamilies(t *testing.T) {
	families := deriveCursorFamilies(catalogOf(
		"grok-4.7-low", "grok-4.7-medium", "grok-4.7-high", "grok-4.7-xhigh",
		"grok-4.7-low-fast", "grok-4.7-medium-fast", "grok-4.7-high-fast", "grok-4.7-xhigh-fast",
		"cursor-grok-4.6-high-fast", "cursor-grok-4.6-xhigh-fast",
		"kimi-k3-max", // single effort: not a family
		"composer-2.5-fast",
		"glm-5.2-high", "glm-5.2-max", "cursor-glm-5.2", // family id already advertised
	))
	got := make([]string, 0, len(families))
	for _, info := range cursorFamilyInfos(families) {
		got = append(got, info.ID)
	}
	want := []string{"cursor-grok-4.6-fast", "cursor-grok-4.7", "cursor-grok-4.7-fast"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("families = %v, want %v", got, want)
	}
	fast := families["cursor-grok-4.7-fast"]
	if fast.Variants["xhigh"] != "grok-4.7-xhigh-fast" || fast.Variants["low"] != "grok-4.7-low-fast" {
		t.Fatalf("fast variants = %v", fast.Variants)
	}
	if levels := fast.levels(); !reflect.DeepEqual(levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("levels = %v", levels)
	}
}

func TestCursorFamilyResolve(t *testing.T) {
	family := deriveCursorFamilies(catalogOf(
		"grok-4.7-low-fast", "grok-4.7-medium-fast", "grok-4.7-high-fast", "grok-4.7-xhigh-fast",
	))["cursor-grok-4.7-fast"]
	cases := map[string]string{
		"low":     "grok-4.7-low-fast",
		"medium":  "grok-4.7-medium-fast",
		"high":    "grok-4.7-high-fast",
		"xhigh":   "grok-4.7-xhigh-fast",
		"max":     "grok-4.7-xhigh-fast", // no max: next weaker
		"minimal": "grok-4.7-low-fast",   // no minimal: next stronger
		"none":    "grok-4.7-low-fast",
		"":        "grok-4.7-medium-fast", // default effort
		"auto":    "grok-4.7-medium-fast",
	}
	for effort, want := range cases {
		if got := family.resolve(effort); got != want {
			t.Errorf("resolve(%q) = %q, want %q", effort, got, want)
		}
	}

	// A family without the default effort still resolves upward first.
	sparse := deriveCursorFamilies(catalogOf("kimi-k3-low", "kimi-k3-high", "kimi-k3-max"))["cursor-kimi-k3"]
	if got := sparse.resolve(""); got != "kimi-k3-high" {
		t.Fatalf("sparse default = %q, want kimi-k3-high", got)
	}
}

func TestCursorFamilyModelInfo(t *testing.T) {
	info := deriveCursorFamilies(catalogOf(
		"gpt-5.6-luna-none", "gpt-5.6-luna-low", "gpt-5.6-luna-max",
	))["cursor-gpt-5.6-luna"].modelInfo()
	if info.ID != "cursor-gpt-5.6-luna" || info.ContextLength != 200000 {
		t.Fatalf("info = %+v", info)
	}
	if info.Thinking == nil || !info.Thinking.ZeroAllowed ||
		!reflect.DeepEqual(info.Thinking.Levels, []string{"none", "low", "max"}) {
		t.Fatalf("thinking = %+v", info.Thinking)
	}
}

func TestResolveCursorModelUsesCachedFamilies(t *testing.T) {
	identity := cursorIdentity{AccessToken: "family-test-token"}
	key := cleanToken(identity.AccessToken)
	storeCursorFamilies(key, deriveCursorFamilies(catalogOf(
		"grok-4.7-low-fast", "grok-4.7-medium-fast", "grok-4.7-high-fast", "grok-4.7-xhigh-fast",
	)))
	t.Cleanup(func() {
		cursorCatalogMu.Lock()
		delete(cursorFamilyCache, key)
		cursorCatalogMu.Unlock()
	})

	cases := []struct{ model, effort, want string }{
		{"cursor-grok-4.7-fast", "low", "grok-4.7-low-fast"},
		{"cursor-grok-4.7-fast", "max", "grok-4.7-xhigh-fast"},
		// Variant ids keep their own effort whatever the request says.
		{"grok-4.7-xhigh-fast", "low", "grok-4.7-xhigh-fast"},
		{"cursor-unknown-fast", "low", "cursor-unknown-fast"},
	}
	for _, c := range cases {
		if got := resolveCursorModel(identity, c.model, c.effort); got != c.want {
			t.Errorf("resolveCursorModel(%q, %q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}
