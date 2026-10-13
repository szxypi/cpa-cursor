package main

// Model aliases: client-facing names for catalog ids, persisted as
// cursor-aliases.yaml in the service working directory. The file is separate
// from cursor-models.yaml on purpose: the panel's reset deletes the selection
// file, and aliases must survive it. Host aliases (oauth-model-alias) cannot
// serve this: the plugin ABI has no callback for editing config.yaml, and the
// host restores the original id before the executor runs, which would bypass
// the effort-family variant selection.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const defaultAliasFileName = "cursor-aliases.yaml"

// cursorAliasEntry maps one original model id to a client-facing alias.
// Fork keeps the original id registered alongside the alias.
type cursorAliasEntry struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork" json:"fork"`
}

// cursorAliasFile is the on-disk shape of cursor-aliases.yaml.
type cursorAliasFile struct {
	Aliases []cursorAliasEntry `yaml:"aliases"`
}

var (
	aliasMu      sync.Mutex
	aliasCache   *cursorAliasFile
	aliasPath    string
	aliasModTime time.Time
)

func aliasFilePath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, defaultAliasFileName), nil
}

// loadCursorAliases re-reads cursor-aliases.yaml when it changed; parse
// errors keep the last good copy so a typo cannot drop every alias.
func loadCursorAliases() []cursorAliasEntry {
	path, err := aliasFilePath()
	if err != nil {
		return nil
	}
	aliasMu.Lock()
	defer aliasMu.Unlock()
	info, statErr := os.Stat(path)
	if statErr != nil {
		aliasCache = &cursorAliasFile{}
		aliasPath = path
		aliasModTime = time.Time{}
		return nil
	}
	parsed := aliasCache
	if aliasPath == path && parsed != nil && info.ModTime().Equal(aliasModTime) {
		return parsed.Aliases
	}
	if aliasPath != path {
		// A different directory (tests chdir around): the cached copy does
		// not belong to this file.
		parsed = nil
	}
	aliasPath = path
	aliasModTime = info.ModTime()
	if raw, readErr := os.ReadFile(path); readErr != nil {
		hostLog("warn", "cannot read "+path+": "+readErr.Error())
	} else {
		fresh := &cursorAliasFile{}
		if err := yaml.Unmarshal(raw, fresh); err != nil {
			hostLog("warn", "ignoring invalid "+path+": "+err.Error())
		} else {
			// A hand-edited file carries no panel validation; drop the broken
			// rows here so they cannot register an empty-id model.
			fresh.Aliases = sanitizeCursorAliases(fresh.Aliases)
			parsed = fresh
		}
	}
	if parsed == nil {
		parsed = &cursorAliasFile{}
	}
	aliasCache = parsed
	return parsed.Aliases
}

// saveCursorAliases persists the entries (0600); an empty list removes the
// file, so "no aliases" stays a missing file.
func saveCursorAliases(entries []cursorAliasEntry) error {
	path, err := aliasFilePath()
	if err != nil {
		return err
	}
	aliasMu.Lock()
	defer aliasMu.Unlock()
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		aliasCache = &cursorAliasFile{}
		aliasPath = path
		aliasModTime = time.Time{}
		return nil
	}
	payload, err := yaml.Marshal(&cursorAliasFile{Aliases: entries})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return err
	}
	info, statErr := os.Stat(path)
	aliasCache = &cursorAliasFile{Aliases: entries}
	aliasPath = path
	if statErr == nil {
		aliasModTime = info.ModTime()
	}
	return nil
}

// aliasForbidden reports whether s carries whitespace or a comma: both break
// the id syntax clients and the catalog use.
func aliasForbidden(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0 || strings.Contains(s, ",")
}

// sanitizeCursorAliases drops rows a hand-edited file can carry but the panel
// never saves: empty name or alias, whitespace or comma, alias equal to name,
// duplicate alias. One warnAliasOnce per problem class keeps the log quiet; a
// broken row drops only itself, not the whole file.
func sanitizeCursorAliases(entries []cursorAliasEntry) []cursorAliasEntry {
	seen := map[string]bool{}
	out := make([]cursorAliasEntry, 0, len(entries))
	for _, e := range entries {
		e.Name = strings.TrimSpace(e.Name)
		e.Alias = strings.TrimSpace(e.Alias)
		switch {
		case e.Name == "" || e.Alias == "":
			warnAliasOnce("dropping alias row with an empty name or alias in " + defaultAliasFileName)
			continue
		case aliasForbidden(e.Name) || aliasForbidden(e.Alias):
			warnAliasOnce("dropping alias row with whitespace or a comma in " + defaultAliasFileName + ": " + e.Alias)
			continue
		case strings.EqualFold(e.Alias, e.Name):
			warnAliasOnce("dropping alias row whose alias equals its name in " + defaultAliasFileName + ": " + e.Alias)
			continue
		}
		key := strings.ToLower(e.Alias)
		if seen[key] {
			warnAliasOnce("dropping duplicate alias in " + defaultAliasFileName + ": " + e.Alias)
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// validateCursorAliasRows trims and checks the panel's rows in order. Fully
// empty rows are dropped. knownIDs carries the catalog ids GET /aliases
// lists, lower-case; an alias matching one of them case-insensitively is
// rejected because the registration path would skip it while the request
// path would rewrite the real model. A nil map skips that check. The line
// number in an error message is the row's 1-based position in the submitted
// array, dropped rows included.
func validateCursorAliasRows(rows []cursorAliasEntry, knownIDs map[string]bool) ([]cursorAliasEntry, error) {
	known := make(map[string]bool, len(knownIDs))
	for id := range knownIDs {
		known[strings.ToLower(id)] = true
	}
	seen := map[string]int{}
	entries := make([]cursorAliasEntry, 0, len(rows))
	for i, row := range rows {
		name := strings.TrimSpace(row.Name)
		alias := strings.TrimSpace(row.Alias)
		if name == "" && alias == "" {
			continue
		}
		line := i + 1
		if name == "" {
			return nil, fmt.Errorf("第 %d 行：原模型名称必填", line)
		}
		if alias == "" {
			return nil, fmt.Errorf("第 %d 行：别名必填", line)
		}
		if aliasForbidden(name) {
			return nil, fmt.Errorf("第 %d 行：原模型名称不能包含空格或逗号", line)
		}
		if aliasForbidden(alias) {
			return nil, fmt.Errorf("第 %d 行：别名不能包含空格或逗号", line)
		}
		if strings.EqualFold(alias, name) {
			return nil, fmt.Errorf("第 %d 行：别名不能与原模型名称相同", line)
		}
		if knownIDs != nil && known[strings.ToLower(alias)] {
			return nil, fmt.Errorf("第 %d 行：别名 %s 与已有模型 ID 冲突", line, alias)
		}
		key := strings.ToLower(alias)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("第 %d 行：别名 %s 与第 %d 行重复", line, alias, prev)
		}
		seen[key] = line
		entries = append(entries, cursorAliasEntry{Name: name, Alias: alias, Fork: row.Fork})
	}
	return entries, nil
}

// resolveCursorAlias maps a configured alias back to its original model id
// (case-insensitive). A requested name that itself matches a known model id
// of the account (case-insensitive) is returned unchanged: the real model
// always wins over an alias spelled like it, so an unregistered conflicting
// alias cannot hijack requests. The known ids come from caches only; this
// runs on every request and must not pull the network. The returned id keeps
// the catalog's canonical casing, which is what goes upstream. Anything that
// matches no alias is returned unchanged, so a family id still flows into the
// effort resolver.
func resolveCursorAlias(identity cursorIdentity, model string) string {
	entries := loadCursorAliases()
	trimmed := strings.TrimSpace(model)
	if len(entries) == 0 || trimmed == "" {
		return model
	}
	var match *cursorAliasEntry
	for i := range entries {
		if strings.EqualFold(trimmed, strings.TrimSpace(entries[i].Alias)) {
			match = &entries[i]
			break
		}
	}
	if match == nil {
		return model
	}
	known := cachedCursorIDIndex(identity, entries)
	if known == nil {
		// No catalog source at all: fall back to the table-only mapping.
		return strings.TrimSpace(match.Name)
	}
	if _, real := known[strings.ToLower(trimmed)]; real {
		return model
	}
	if canonical, ok := known[strings.ToLower(strings.TrimSpace(match.Name))]; ok {
		return canonical
	}
	return strings.TrimSpace(match.Name)
}

// cachedCursorIDIndex lists every model id the account is known to carry
// without touching the network: the merged-catalog memory cache (its alias
// registrations excluded), the account catalog saved on disk, the effort
// family cache and the static list. Keys are lower-case ids, values keep the
// catalog's canonical casing. A nil map means no source carried anything.
func cachedCursorIDIndex(identity cursorIdentity, aliases []cursorAliasEntry) map[string]string {
	key := cleanToken(identity.AccessToken)
	index := map[string]string{}
	cursorCatalogMu.Lock()
	entry, cached := cursorCatalogCache[key]
	cursorCatalogMu.Unlock()
	if cached {
		for _, m := range entry.models {
			if aliasListed(aliases, m.ID) {
				// The overlay registered this entry as an alias, not as a
				// real catalog id.
				continue
			}
			index[strings.ToLower(m.ID)] = m.ID
		}
	}
	for _, m := range loadCursorCatalogDisk(key) {
		index[strings.ToLower(m.ID)] = m.ID
	}
	for _, family := range cachedCursorFamilies(key) {
		index[strings.ToLower(family.ID)] = family.ID
		for _, variant := range family.Variants {
			index[strings.ToLower(variant)] = variant
		}
	}
	for _, m := range staticCursorModels {
		index[strings.ToLower(m.ID)] = m.ID
	}
	if len(index) == 0 {
		return nil
	}
	return index
}

// aliasListed reports whether id equals one of the configured aliases,
// case-insensitively.
func aliasListed(entries []cursorAliasEntry, id string) bool {
	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e.Alias), id) {
			return true
		}
	}
	return false
}

// aliasWarnOnce keeps one warn per distinct alias problem, so a rebuild every
// cache TTL cannot flood the journal.
var aliasWarnOnce sync.Map

func warnAliasOnce(message string) {
	if _, dup := aliasWarnOnce.LoadOrStore(message, true); !dup {
		hostLog("warn", message)
	}
}

// appendCursorAliases overlays the configured aliases onto the filtered
// catalog. Sources come from source, the account's unfiltered catalog
// (families included), so an alias still registers when the original id is
// not selected. Any fork:false entry on a name hides that original id. Names
// normalize case-insensitively to the catalog's own id, so {Name:"Live-Model"}
// hides and copies live-model; a name the source does not carry hides nothing.
func appendCursorAliases(filtered, source []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	entries := loadCursorAliases()
	if len(entries) == 0 {
		return filtered
	}
	byID := make(map[string]pluginapi.ModelInfo, len(source))
	for _, m := range source {
		byID[strings.ToLower(m.ID)] = m
	}
	hidden := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Fork {
			continue
		}
		// A source the catalog does not carry hides nothing: a typo in the
		// name must not remove a real model from the list. The warn lives in
		// the registration loop, which sees fork:true entries too.
		if _, ok := byID[strings.ToLower(e.Name)]; ok {
			hidden[strings.ToLower(e.Name)] = true
		}
	}
	out := make([]pluginapi.ModelInfo, 0, len(filtered)+len(entries))
	listed := make(map[string]bool, len(filtered)+len(entries))
	for _, m := range filtered {
		if hidden[strings.ToLower(m.ID)] {
			continue
		}
		out = append(out, m)
		listed[strings.ToLower(m.ID)] = true
	}
	for _, e := range entries {
		src, ok := byID[strings.ToLower(e.Name)]
		if !ok {
			warnAliasOnce("alias source not in the account catalog, skipped: " + e.Name)
			continue
		}
		key := strings.ToLower(e.Alias)
		if listed[key] {
			warnAliasOnce("alias conflicts with a registered model id, skipped: " + e.Alias)
			continue
		}
		clone := src
		clone.ID = e.Alias
		clone.Name = e.Alias
		clone.DisplayName = e.Alias
		out = append(out, clone)
		listed[key] = true
	}
	return out
}

// importedCursorCredentials scans the auths directory for cursor credential
// files, the same set rewriteCursorAuths covers. host.auth.list hides file
// credentials without a path attribute, so the directory is read directly.
func importedCursorCredentials() []cursorCredential {
	matches, err := filepath.Glob(filepath.Join(cwdOrRoot(), "auths", providerKey+"-*.json"))
	if err != nil {
		return nil
	}
	creds := make([]cursorCredential, 0, len(matches))
	for _, path := range matches {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		cred, errParse := parseCursorCredential(raw)
		if errParse != nil || cred.AccessToken == "" {
			continue
		}
		creds = append(creds, *cred)
	}
	return creds
}

// unfilteredCatalogForToken is the alias picker's source: the account's full
// merged catalog (live, disk copy or static fallback) before the policy
// filter, effort families included.
func unfilteredCatalogForToken(identity cursorIdentity) []pluginapi.ModelInfo {
	key := cleanToken(identity.AccessToken)
	return buildCursorCatalog(key, accountCatalog(identity, key))
}

// knownModelIDs collects every id an alias may point at or clash with: all
// accounts' unfiltered catalogs plus the static fallback when no account is
// reachable. Same source as GET /aliases, ids keep the catalog casing. It
// may pull live catalogs, same cost as GET /aliases; callers keep it out of
// the pure row validation.
func knownModelIDs() map[string]bool {
	ids := map[string]bool{}
	for _, cred := range importedCursorCredentials() {
		for _, m := range unfilteredCatalogForToken(cred.identity()) {
			ids[m.ID] = true
		}
	}
	if len(ids) == 0 {
		for _, m := range staticCursorModels {
			ids[m.ID] = true
		}
	}
	return ids
}

// managementAliases answers GET /aliases: the saved entries plus every id an
// alias may point at, deduped and sorted.
func managementAliases() ([]byte, error) {
	entries := loadCursorAliases()
	if entries == nil {
		entries = []cursorAliasEntry{}
	}
	ids := knownModelIDs()
	models := make([]string, 0, len(ids))
	for id := range ids {
		models = append(models, id)
	}
	sort.Strings(models)
	return jsonResponse(http.StatusOK, map[string]any{"aliases": entries, "models": models})
}

// managementAliasSet answers GET /aliases/set?data=<url-encoded JSON array>.
// The host only dispatches GET to resource routes, so the save travels as a
// query like /models/set.
func managementAliasSet(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"status": "error", "error": "data 参数不能为空"})
	}
	var rows []cursorAliasEntry
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"status": "error", "error": "data 不是合法的 JSON 数组：" + err.Error()})
	}
	// json.Unmarshal("null", &rows) succeeds with rows == nil; only an actual
	// empty array may clear the file.
	if rows == nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"status": "error", "error": "data 不是合法的 JSON 数组"})
	}
	entries, err := validateCursorAliasRows(rows, knownModelIDs())
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
	}
	if err := saveCursorAliases(entries); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"status": "error", "error": "保存别名失败：" + err.Error()})
	}
	// The merged catalog was overlaid with the previous aliases; drop it so
	// the host's re-pull sees the new ones.
	clearCursorCatalogCache()
	rewritten := rewriteCursorAuths()
	return jsonResponse(http.StatusOK, map[string]any{"status": "success", "count": len(entries), "refreshed": rewritten})
}
