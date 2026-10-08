package main

// Effort families. Cursor advertises one model ID per reasoning effort (for
// example grok-4.7-low-fast … grok-4.7-xhigh-fast), and the AgentService run
// request carries only that ID, so the only way to choose an effort is to
// choose the ID. A family ID (cursor-grok-4.7-fast) stands for the whole set:
// the executor picks the variant from the request's reasoning_effort.

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// cursorEffortOrder lists Cursor's effort suffixes from weakest to strongest.
var cursorEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// cursorDefaultEffort applies when the request carries no known effort.
const cursorDefaultEffort = "medium"

// cursorFamilyPrefix keeps family IDs apart from other providers' models:
// CPA merges same-named models from different providers into one pool, and
// xai already registers grok-4.7.
const cursorFamilyPrefix = "cursor-"

// cursorFamily is one set of advertised variants that differ only in effort.
type cursorFamily struct {
	ID       string
	Base     string
	Fast     bool
	Variants map[string]string // effort -> advertised variant id
}

// cursorFamilyCache keeps the families of the last successful live catalog
// per token, so a failed refresh does not take the families offline.
// cursorCatalogMu guards it.
var cursorFamilyCache = map[string]map[string]cursorFamily{}

func effortRank(effort string) int {
	for i, e := range cursorEffortOrder {
		if e == effort {
			return i
		}
	}
	return -1
}

// splitCursorVariant splits a "<base>-<effort>[-fast]" model id.
func splitCursorVariant(id string) (base, effort string, fast, ok bool) {
	rest := id
	if strings.HasSuffix(rest, "-fast") {
		rest = strings.TrimSuffix(rest, "-fast")
		fast = true
	}
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return "", "", false, false
	}
	base, effort = rest[:idx], rest[idx+1:]
	if effortRank(effort) < 0 {
		return "", "", false, false
	}
	return base, effort, fast, true
}

func cursorFamilyID(base string, fast bool) string {
	id := base
	if !strings.HasPrefix(id, cursorFamilyPrefix) {
		id = cursorFamilyPrefix + id
	}
	if fast {
		id += "-fast"
	}
	return id
}

// deriveCursorFamilies groups a catalog into families of two or more
// efforts. A family whose id is itself an advertised model is skipped.
func deriveCursorFamilies(models []pluginapi.ModelInfo) map[string]cursorFamily {
	advertised := make(map[string]bool, len(models))
	for _, m := range models {
		advertised[m.ID] = true
	}
	families := map[string]cursorFamily{}
	for _, m := range models {
		base, effort, fast, ok := splitCursorVariant(m.ID)
		if !ok {
			continue
		}
		id := cursorFamilyID(base, fast)
		family, exists := families[id]
		if !exists {
			family = cursorFamily{ID: id, Base: base, Fast: fast, Variants: map[string]string{}}
		}
		family.Variants[effort] = m.ID
		families[id] = family
	}
	for id, family := range families {
		if len(family.Variants) < 2 || advertised[id] {
			delete(families, id)
		}
	}
	return families
}

// levels returns the family's efforts from weakest to strongest.
func (f cursorFamily) levels() []string {
	out := make([]string, 0, len(f.Variants))
	for _, effort := range cursorEffortOrder {
		if _, ok := f.Variants[effort]; ok {
			out = append(out, effort)
		}
	}
	return out
}

// resolve returns the variant for effort. An unknown or empty effort means
// cursorDefaultEffort. A missing effort takes the next stronger variant, then
// the next weaker one, which is how pi clamps thinking levels.
func (f cursorFamily) resolve(effort string) string {
	want := effortRank(effort)
	if want < 0 {
		want = effortRank(cursorDefaultEffort)
	}
	for r := want; r < len(cursorEffortOrder); r++ {
		if id, ok := f.Variants[cursorEffortOrder[r]]; ok {
			return id
		}
	}
	for r := want - 1; r >= 0; r-- {
		if id, ok := f.Variants[cursorEffortOrder[r]]; ok {
			return id
		}
	}
	return ""
}

// modelInfo describes the family in /v1/models. Thinking.Levels lists the
// efforts the family can select.
func (f cursorFamily) modelInfo() pluginapi.ModelInfo {
	levels := f.levels()
	// Base keeps the upstream id's own cursor- prefix, which the display
	// already carries; strip it so the label is not "Cursor cursor-...".
	name := strings.TrimPrefix(f.Base, cursorFamilyPrefix)
	if f.Fast {
		name += " fast"
	}
	infos := []pluginapi.ModelInfo{{
		ID:          f.ID,
		Name:        f.ID,
		DisplayName: "Cursor " + name + " (effort family)",
		Description: "efforts: " + strings.Join(levels, ", "),
	}}
	applyCursorModelTiers(infos)
	_, zeroAllowed := f.Variants["none"]
	infos[0].Thinking = &pluginapi.ThinkingSupport{
		ZeroAllowed:    zeroAllowed,
		DynamicAllowed: true,
		Levels:         levels,
	}
	return infos[0]
}

// cursorFamilyInfos returns the families' model entries sorted by id.
func cursorFamilyInfos(families map[string]cursorFamily) []pluginapi.ModelInfo {
	ids := make([]string, 0, len(families))
	for id := range families {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, families[id].modelInfo())
	}
	return out
}

func storeCursorFamilies(key string, families map[string]cursorFamily) {
	cursorCatalogMu.Lock()
	cursorFamilyCache[key] = families
	cursorCatalogMu.Unlock()
}

func cachedCursorFamilies(key string) map[string]cursorFamily {
	cursorCatalogMu.Lock()
	defer cursorCatalogMu.Unlock()
	return cursorFamilyCache[key]
}

// resolveCursorModel maps a family id to the account's variant for effort.
// Any other id is returned unchanged. Variants come from the full account
// catalog: the panel selection limits which families are listed, not which
// efforts a listed family may use.
func resolveCursorModel(identity cursorIdentity, model, effort string) string {
	key := cleanToken(identity.AccessToken)
	families := cachedCursorFamilies(key)
	if families == nil && strings.HasPrefix(model, cursorFamilyPrefix) {
		catalogForToken(identity)
		families = cachedCursorFamilies(key)
	}
	if family, ok := families[model]; ok {
		if variant := family.resolve(effort); variant != "" {
			return variant
		}
	}
	return model
}
