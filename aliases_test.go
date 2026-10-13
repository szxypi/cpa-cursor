package main

// Model-alias tests: validation, persistence, catalog registration, request
// resolution and the management endpoints. Every test works in a temporary
// working directory, the same isolation models_test.go uses.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// resetAliasCache clears the alias cache so tests do not see each other's
// files.
func resetAliasCache() {
	aliasMu.Lock()
	aliasCache = nil
	aliasPath = ""
	aliasModTime = time.Time{}
	aliasMu.Unlock()
	aliasWarnOnce = sync.Map{}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ==================== validation ====================

// TestValidateCursorAliasRows covers trimming, dropped empty rows and order
// preservation.
func TestValidateCursorAliasRows(t *testing.T) {
	valid, err := validateCursorAliasRows([]cursorAliasEntry{
		{Name: " grok-4.7-xhigh-fast ", Alias: " my-grok "},
		{Name: "", Alias: ""},    // dropped
		{Name: "\t", Alias: " "}, // dropped
		{Name: "cursor-grok-4.7-fast", Alias: "fam", Fork: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []cursorAliasEntry{
		{Name: "grok-4.7-xhigh-fast", Alias: "my-grok"},
		{Name: "cursor-grok-4.7-fast", Alias: "fam", Fork: true},
	}
	if !reflect.DeepEqual(valid, want) {
		t.Fatalf("entries = %+v, want %+v", valid, want)
	}
}

// TestValidateCursorAliasRowsErrors covers every rejection with its line
// number. Line numbers count dropped rows too.
func TestValidateCursorAliasRowsErrors(t *testing.T) {
	cases := []struct {
		name string
		rows []cursorAliasEntry
		want string
	}{
		{"alias required", []cursorAliasEntry{{Name: "m"}}, "第 1 行：别名必填"},
		{"name required", []cursorAliasEntry{{Name: "", Alias: "x"}}, "第 1 行：原模型名称必填"},
		{
			"name required after dropped row",
			[]cursorAliasEntry{{Name: "m", Alias: "a"}, {Name: "", Alias: ""}, {Name: "", Alias: "b"}},
			"第 3 行：原模型名称必填",
		},
		{"whitespace in name", []cursorAliasEntry{{Name: "a b", Alias: "x"}}, "第 1 行：原模型名称不能包含空格或逗号"},
		{"comma in name", []cursorAliasEntry{{Name: "a,b", Alias: "x"}}, "第 1 行：原模型名称不能包含空格或逗号"},
		{"whitespace in alias", []cursorAliasEntry{{Name: "a", Alias: "x y"}}, "第 1 行：别名不能包含空格或逗号"},
		{"comma in alias", []cursorAliasEntry{{Name: "a", Alias: "x,y"}}, "第 1 行：别名不能包含空格或逗号"},
		{"alias equals name", []cursorAliasEntry{{Name: "model", Alias: "MODEL"}}, "第 1 行：别名不能与原模型名称相同"},
		{
			"duplicate alias",
			[]cursorAliasEntry{{Name: "m1", Alias: "dup"}, {Name: "m2", Alias: "Dup"}},
			"第 2 行：别名 Dup 与第 1 行重复",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validateCursorAliasRows(c.rows, nil)
			if err == nil {
				t.Fatal("validation unexpectedly passed")
			}
			if err.Error() != c.want {
				t.Fatalf("error = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

// TestValidateCursorAliasRowsIDConflict covers the catalog-id clash: an alias
// that matches a known model id case-insensitively is rejected with the row
// number. A nil id set (pure row validation) skips the check.
func TestValidateCursorAliasRowsIDConflict(t *testing.T) {
	known := map[string]bool{"live-model": true, "GROK-4.7-XHIGH-FAST": true}
	cases := []struct {
		name string
		rows []cursorAliasEntry
		want string
	}{
		{
			"same casing",
			[]cursorAliasEntry{{Name: "live-model", Alias: "my-live"}, {Name: "other", Alias: "live-model"}},
			"第 2 行：别名 live-model 与已有模型 ID 冲突",
		},
		{
			"upper-cased alias against a lower-case id",
			[]cursorAliasEntry{{Name: "live-model", Alias: "grok-4.7-xhigh-fast"}},
			"第 1 行：别名 grok-4.7-xhigh-fast 与已有模型 ID 冲突",
		},
		{
			"alias equals its own name fires first",
			[]cursorAliasEntry{{Name: "live-model", Alias: "live-model"}},
			"第 1 行：别名不能与原模型名称相同",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validateCursorAliasRows(c.rows, known)
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}

	valid, err := validateCursorAliasRows([]cursorAliasEntry{{Name: "live-model", Alias: "my-live"}}, known)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].Alias != "my-live" {
		t.Fatalf("valid = %+v", valid)
	}
	if _, err := validateCursorAliasRows([]cursorAliasEntry{{Name: "live-model", Alias: "live-model-x"}}, nil); err != nil {
		t.Fatalf("nil id set rejected a clean row: %v", err)
	}
}

// ==================== persistence ====================

// TestSaveCursorAliasesRoundTrip covers the save/reload cycle, the file mode
// and the empty-save file removal.
func TestSaveCursorAliasesRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	resetAliasCache()
	entries := []cursorAliasEntry{
		{Name: "grok-4.7-xhigh-fast", Alias: "my-grok"},
		{Name: "cursor-grok-4.6-fast", Alias: "fam", Fork: true},
	}
	if err := saveCursorAliases(entries); err != nil {
		t.Fatal(err)
	}
	path, err := aliasFilePath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if got := loadCursorAliases(); !reflect.DeepEqual(got, entries) {
		t.Fatalf("reloaded = %+v, want %+v", got, entries)
	}

	// An empty save removes the file: "no aliases" stays a missing file.
	if err := saveCursorAliases(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("alias file still present: %v", err)
	}
	if got := loadCursorAliases(); len(got) != 0 {
		t.Fatalf("aliases survived the empty save: %+v", got)
	}
}

// TestLoadCursorAliasesKeepsLastGoodCopy covers a corrupt file: the previous
// copy stays until a valid file replaces it.
func TestLoadCursorAliasesKeepsLastGoodCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	resetAliasCache()
	good := []cursorAliasEntry{{Name: "live-model", Alias: "my-live"}}
	if err := saveCursorAliases(good); err != nil {
		t.Fatal(err)
	}
	path, err := aliasFilePath()
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the file. Chtimes guarantees a new mtime even on filesystems
	// with coarse timestamps.
	if err := os.WriteFile(path, []byte("aliases: [ {"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got := loadCursorAliases(); !reflect.DeepEqual(got, good) {
		t.Fatalf("corrupt file dropped the aliases: %+v", got)
	}
	// The broken file does not reload on every call.
	if got := loadCursorAliases(); !reflect.DeepEqual(got, good) {
		t.Fatalf("second load changed the aliases: %+v", got)
	}

	// A fixed file is picked up again.
	fixed := &cursorAliasFile{Aliases: []cursorAliasEntry{
		{Name: "a", Alias: "b"},
		{Name: "c", Alias: "d"},
	}}
	payload, err := yaml.Marshal(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	later := future.Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if got := loadCursorAliases(); !reflect.DeepEqual(got, fixed.Aliases) {
		t.Fatalf("fixed file not reloaded: %+v", got)
	}
}

// ==================== registration ====================

// TestLoadCursorAliasesSanitizesHandEdits covers a hand-edited file: rows the
// panel never saves (empty fields, whitespace, alias equal to name, duplicate
// alias) drop individually, and the valid row survives. No broken row can
// register an empty-id model.
func TestLoadCursorAliasesSanitizesHandEdits(t *testing.T) {
	t.Chdir(t.TempDir())
	resetAliasCache()
	path, err := aliasFilePath()
	if err != nil {
		t.Fatal(err)
	}
	hand := "aliases:\n" +
		"  - name: live-model\n" +
		"    alias: my-live\n" +
		"  - name: no-alias\n" +
		"  - name: a b\n" +
		"    alias: has space\n" +
		"  - name: same\n" +
		"    alias: SAME\n" +
		"  - name: other\n" +
		"    alias: my-live\n"
	if err := os.WriteFile(path, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []cursorAliasEntry{{Name: "live-model", Alias: "my-live"}}
	if got := loadCursorAliases(); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded = %+v, want %+v", got, want)
	}
	models := appendCursorAliases(nil, catalogOf("live-model"))
	if hasModelID(models, "") {
		t.Fatal("a broken row registered an empty model id")
	}
	if !hasModelID(models, "my-live") {
		t.Fatal("the valid row was dropped with the broken ones")
	}
}

// aliasCatalogEnv prepares a temp cwd with a fixed live catalog and saved
// aliases, and returns the identity to pull the catalog with.
func aliasCatalogEnv(t *testing.T, entries []cursorAliasEntry) cursorIdentity {
	t.Helper()
	t.Chdir(t.TempDir())
	resetCursorCaches()
	resetAliasCache()
	stubLiveCatalog(t, []pluginapi.ModelInfo{
		{ID: "grok-4.7-low-fast", Name: "grok-4.7-low-fast", DisplayName: "Grok 4.7 Low Fast"},
		{ID: "grok-4.7-xhigh-fast", Name: "grok-4.7-xhigh-fast", DisplayName: "Grok 4.7 XHigh Fast"},
		{ID: "live-model", Name: "live-model", DisplayName: "Live Model"},
	})
	if err := saveCursorAliases(entries); err != nil {
		t.Fatal(err)
	}
	return cursorIdentity{AccessToken: "tok-" + t.Name()}
}

// TestCatalogAliasForkTrue covers fork:true: the original id and the alias
// are both registered, and the alias copies the source entry's metadata.
func TestCatalogAliasForkTrue(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "live-model", Alias: "my-live", Fork: true}})
	models := catalogForToken(identity)
	if !hasModelID(models, "live-model") {
		t.Fatal("fork:true dropped the original id")
	}
	if !hasModelID(models, "my-live") {
		t.Fatal("alias missing from the catalog")
	}
	var alias pluginapi.ModelInfo
	for _, m := range models {
		if m.ID == "my-live" {
			alias = m
		}
	}
	if alias.Name != "my-live" || alias.DisplayName != "my-live" {
		t.Fatalf("alias entry names = %+v", alias)
	}
	if alias.Type != "chat" || alias.Object != "model" {
		t.Fatalf("alias entry lost the source tiers: %+v", alias)
	}
}

// TestCatalogAliasForkFalse covers fork:false: only the alias registers.
func TestCatalogAliasForkFalse(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "live-model", Alias: "my-live", Fork: false}})
	models := catalogForToken(identity)
	if hasModelID(models, "live-model") {
		t.Fatal("fork:false kept the original id")
	}
	if !hasModelID(models, "my-live") {
		t.Fatal("alias missing from the catalog")
	}
}

// TestCatalogAliasForUnselectedSource covers a source the policy filter
// removed: the user configured the alias, so it still registers.
func TestCatalogAliasForUnselectedSource(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "live-model", Alias: "my-live", Fork: true}})
	if err := savePolicySelection([]string{"auto"}); err != nil {
		t.Fatal(err)
	}
	models := catalogForToken(identity)
	if !hasModelID(models, "my-live") {
		t.Fatal("alias for an unselected source missing")
	}
	if hasModelID(models, "live-model") {
		t.Fatal("unselected original id leaked")
	}
	if !hasModelID(models, "auto") {
		t.Fatal("selected model missing")
	}
}

// TestCatalogAliasCaseInsensitiveSource covers a source name whose casing
// differs from the catalog id: fork:false still hides the original id, the
// alias registers, and the resolver returns the catalog's canonical casing.
func TestCatalogAliasCaseInsensitiveSource(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "Live-Model", Alias: "my-live", Fork: false}})
	models := catalogForToken(identity)
	if hasModelID(models, "live-model") {
		t.Fatal("mixed-case source kept the original id on fork:false")
	}
	if !hasModelID(models, "my-live") {
		t.Fatal("mixed-case source dropped the alias")
	}
	if got := resolveCursorAlias(identity, "my-live"); got != "live-model" {
		t.Fatalf("resolve(my-live) = %q, want the canonical live-model", got)
	}
}

// TestCatalogAliasMissingSourceHidesNothing covers a fork:false name the
// catalog does not carry: no model disappears because of the typo.
func TestCatalogAliasMissingSourceHidesNothing(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "ghost-model", Alias: "my-live", Fork: false}})
	models := catalogForToken(identity)
	if !hasModelID(models, "live-model") {
		t.Fatal("missing source hid an unrelated model")
	}
	if hasModelID(models, "my-live") {
		t.Fatal("alias for a missing source registered")
	}
}

// TestCatalogAliasMissingSourceSkipped covers a name no account carries: the
// alias is skipped without breaking the rest.
func TestCatalogAliasMissingSourceSkipped(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{
		{Name: "ghost-model", Alias: "ghost"},
		{Name: "live-model", Alias: "my-live"},
	})
	models := catalogForToken(identity)
	if hasModelID(models, "ghost") {
		t.Fatal("alias for a missing source registered")
	}
	if !hasModelID(models, "my-live") {
		t.Fatal("valid alias skipped after a missing source")
	}
}

// TestCatalogAliasConflictSkipped covers an alias that collides with an
// already registered id, case-insensitively. The real model must also win on
// the request path: resolving the colliding name maps it back to itself, not
// to the alias' source.
func TestCatalogAliasConflictSkipped(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{
		{Name: "grok-4.7-low-fast", Alias: "grok-4.7-xhigh-fast"},
		{Name: "live-model", Alias: "GROK-4.7-XHIGH-FAST"},
	})
	models := catalogForToken(identity)
	if n := countModelID(models, "grok-4.7-xhigh-fast"); n != 1 {
		t.Fatalf("conflicting alias duplicated the id %d times", n)
	}
	if hasModelID(models, "GROK-4.7-XHIGH-FAST") {
		t.Fatal("case-insensitive conflict registered")
	}
	// The skipped aliases never reach the request path either: a request for
	// the real model id must not rewrite to the alias' source.
	if got := resolveCursorAlias(identity, "grok-4.7-xhigh-fast"); got != "grok-4.7-xhigh-fast" {
		t.Fatalf("resolve(grok-4.7-xhigh-fast) = %q, want the id unchanged", got)
	}
	if got := resolveCursorAlias(identity, "GROK-4.7-XHIGH-FAST"); got != "GROK-4.7-XHIGH-FAST" {
		t.Fatalf("resolve(GROK-4.7-XHIGH-FAST) = %q, want the id unchanged", got)
	}
}

// TestCatalogAliasOnFamilyID covers an alias whose source is a family id:
// the alias registers and still resolves through the effort variants.
func TestCatalogAliasOnFamilyID(t *testing.T) {
	identity := aliasCatalogEnv(t, []cursorAliasEntry{{Name: "cursor-grok-4.7-fast", Alias: "my-grok", Fork: false}})
	models := catalogForToken(identity)
	if !hasModelID(models, "my-grok") {
		t.Fatal("family alias missing from the catalog")
	}
	if hasModelID(models, "cursor-grok-4.7-fast") {
		t.Fatal("fork:false kept the family id")
	}
	if got := resolveCursorModel(identity, resolveCursorAlias(identity, "MY-GROK"), "xhigh"); got != "grok-4.7-xhigh-fast" {
		t.Fatalf("family alias resolved to %q, want grok-4.7-xhigh-fast", got)
	}
}

// ==================== resolution ====================

// TestResolveCursorAliasBeforeFamilyResolution mirrors
// TestResolveCursorModelUsesCachedFamilies: an alias maps back to its family
// id, then the effort picks the variant. Unknown ids pass through.
func TestResolveCursorAliasBeforeFamilyResolution(t *testing.T) {
	t.Chdir(t.TempDir())
	resetAliasCache()
	identity := cursorIdentity{AccessToken: "pf-key-alias-resolve"}
	key := cleanToken(identity.AccessToken)
	storeCursorFamilies(key, deriveCursorFamilies(catalogOf(
		"grok-4.7-low-fast", "grok-4.7-xhigh-fast",
	)))
	t.Cleanup(func() {
		cursorCatalogMu.Lock()
		delete(cursorFamilyCache, key)
		cursorCatalogMu.Unlock()
	})
	if err := saveCursorAliases([]cursorAliasEntry{{Name: "cursor-grok-4.7-fast", Alias: "my-grok"}}); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ model, effort, want string }{
		{"my-grok", "xhigh", "grok-4.7-xhigh-fast"},
		{"MY-GROK", "low", "grok-4.7-low-fast"},
		{"my-grok", "", "grok-4.7-xhigh-fast"},              // default effort: no medium, next stronger
		{"grok-4.7-low-fast", "xhigh", "grok-4.7-low-fast"}, // variant ids keep their own effort
		{"unknown-model", "", "unknown-model"},              // not an alias: unchanged
	}
	for _, c := range cases {
		if got := resolveCursorModel(identity, resolveCursorAlias(identity, c.model), c.effort); got != c.want {
			t.Errorf("resolve(%q, %q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}

// ==================== management endpoints ====================

func decodeManagementResponse(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return env.Result
}

func managementCall(t *testing.T, path string, query map[string][]string) pluginapi.ManagementResponse {
	t.Helper()
	request, err := json.Marshal(map[string]any{"Path": path, "Query": query})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleManagementRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return decodeManagementResponse(t, raw)
}

// TestManagementAliasesShape covers GET /aliases: non-null aliases and
// models, sorted ids, the static fallback with no account, and the account's
// unfiltered catalog with families once one is imported.
func TestManagementAliasesShape(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	resetCursorCaches()
	resetAliasCache()

	// No accounts yet: the static catalog names the valid alias targets.
	resp := managementCall(t, "/aliases", map[string][]string{})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(string(resp.Body), "null") {
		t.Fatalf("body carries a null field: %s", resp.Body)
	}
	var body struct {
		Aliases []cursorAliasEntry `json:"aliases"`
		Models  []string           `json:"models"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Aliases == nil || len(body.Aliases) != 0 {
		t.Fatalf("aliases = %#v, want empty non-null", body.Aliases)
	}
	if body.Models == nil || len(body.Models) == 0 {
		t.Fatalf("models = %#v, want the static ids", body.Models)
	}
	if !sort.StringsAreSorted(body.Models) {
		t.Fatalf("models not sorted: %v", body.Models)
	}
	if !containsString(body.Models, "auto") {
		t.Fatalf("static id missing: %v", body.Models)
	}

	// One account: the unfiltered live catalog and its family ids appear,
	// and the saved aliases come back in order.
	authsDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := newCursorCredential("tok-alias-shape", "machine-1", "a@b.c")
	storage, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authsDir, cred.fileName()), storage, 0o600); err != nil {
		t.Fatal(err)
	}
	stubLiveCatalog(t, []pluginapi.ModelInfo{
		{ID: "grok-4.7-low-fast", Name: "grok-4.7-low-fast"},
		{ID: "grok-4.7-xhigh-fast", Name: "grok-4.7-xhigh-fast"},
		{ID: "acct-model", Name: "acct-model"},
	})
	if err := saveCursorAliases([]cursorAliasEntry{{Name: "acct-model", Alias: "my-acct", Fork: true}}); err != nil {
		t.Fatal(err)
	}

	resp = managementCall(t, "/aliases", map[string][]string{})
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := cursorAliasEntry{Name: "acct-model", Alias: "my-acct", Fork: true}
	if len(body.Aliases) != 1 || body.Aliases[0] != want {
		t.Fatalf("aliases = %+v, want [%+v]", body.Aliases, want)
	}
	for _, id := range []string{"acct-model", "cursor-grok-4.7-fast", "grok-4.7-low-fast", "auto"} {
		if !containsString(body.Models, id) {
			t.Fatalf("models missing %q: %v", id, body.Models)
		}
	}
	if !sort.StringsAreSorted(body.Models) {
		t.Fatalf("models not sorted: %v", body.Models)
	}
}

// TestManagementAliasSet covers the save endpoint: the success shape, the
// cleared catalog cache, the 400 shapes, and the empty-array file removal.
func TestManagementAliasSet(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	resetCursorCaches()
	resetAliasCache()
	stubLiveCatalog(t, []pluginapi.ModelInfo{{ID: "live-model", Name: "live-model"}})

	// One imported account, so the save reports refreshed = 1 like
	// /models/set does.
	authsDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := newCursorCredential("tok-alias-set", "machine-1", "")
	storage, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authsDir, cred.fileName()), storage, 0o600); err != nil {
		t.Fatal(err)
	}

	// Prime the merged catalog; the save must drop it (same contract as
	// /models/set).
	identity := cursorIdentity{AccessToken: "tok-alias-set"}
	catalogForToken(identity)
	cursorCatalogMu.Lock()
	before := len(cursorCatalogCache)
	cursorCatalogMu.Unlock()
	if before == 0 {
		t.Fatal("cache was not primed")
	}

	resp := managementCall(t, "/aliases/set", map[string][]string{
		"data": {`[{"name":"live-model","alias":"my-live","fork":true}]`},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
	}
	var saved struct {
		Status    string `json:"status"`
		Count     int    `json:"count"`
		Refreshed int    `json:"refreshed"`
	}
	if err := json.Unmarshal(resp.Body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "success" || saved.Count != 1 || saved.Refreshed != 1 {
		t.Fatalf("body = %+v", saved)
	}
	cursorCatalogMu.Lock()
	after := len(cursorCatalogCache)
	cursorCatalogMu.Unlock()
	if after != 0 {
		t.Fatalf("cache still holds %d entries after an alias save", after)
	}
	if got := loadCursorAliases(); len(got) != 1 || got[0].Name != "live-model" || got[0].Alias != "my-live" || !got[0].Fork {
		t.Fatalf("saved aliases = %+v", got)
	}
	path, err := aliasFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("alias file not written: %v", err)
	}

	// Validation failure: 400 with the row number in the message.
	resp = managementCall(t, "/aliases/set", map[string][]string{
		"data": {`[{"name":"m1","alias":"dup"},{"name":"m2","alias":"dup"}]`},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
	}
	var failed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "error" || failed.Error != "第 2 行：别名 dup 与第 1 行重复" {
		t.Fatalf("error body = %+v", failed)
	}
	// The failed save keeps the previous file.
	if got := loadCursorAliases(); len(got) != 1 {
		t.Fatalf("failed save changed the aliases: %+v", got)
	}

	// Invalid JSON and a missing data parameter are 400s, and neither wipes
	// the saved file.
	resp = managementCall(t, "/aliases/set", map[string][]string{"data": {"not-json"}})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid json status = %d, body %s", resp.StatusCode, resp.Body)
	}
	resp = managementCall(t, "/aliases/set", map[string][]string{})
	if resp.StatusCode != 400 {
		t.Fatalf("missing data status = %d, body %s", resp.StatusCode, resp.Body)
	}
	// data=null unmarshals into a nil slice without an error; only a real
	// empty array may clear the file.
	resp = managementCall(t, "/aliases/set", map[string][]string{"data": {"null"}})
	if resp.StatusCode != 400 {
		t.Fatalf("null data status = %d, body %s", resp.StatusCode, resp.Body)
	}
	if err := json.Unmarshal(resp.Body, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "error" || failed.Error != "data 不是合法的 JSON 数组" {
		t.Fatalf("null data error body = %+v", failed)
	}
	if got := loadCursorAliases(); len(got) != 1 {
		t.Fatalf("a rejected save wiped the aliases: %+v", got)
	}

	// An empty array deletes the file.
	resp = managementCall(t, "/aliases/set", map[string][]string{"data": {"[]"}})
	if resp.StatusCode != 200 {
		t.Fatalf("empty save status = %d, body %s", resp.StatusCode, resp.Body)
	}
	if err := json.Unmarshal(resp.Body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "success" || saved.Count != 0 {
		t.Fatalf("empty save body = %+v", saved)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("alias file still present: %v", err)
	}
}

// TestManagementAliasSetIDConflict covers the save endpoint's catalog check:
// an alias clashing with a model id GET /aliases lists (the account's live
// catalog here) is a 400, and the rejected save writes nothing.
func TestManagementAliasSetIDConflict(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	resetCursorCaches()
	resetAliasCache()
	stubLiveCatalog(t, []pluginapi.ModelInfo{
		{ID: "live-model", Name: "live-model"},
		{ID: "grok-4.7-xhigh-fast", Name: "grok-4.7-xhigh-fast"},
	})
	authsDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := newCursorCredential("tok-alias-conflict", "machine-1", "")
	storage, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authsDir, cred.fileName()), storage, 0o600); err != nil {
		t.Fatal(err)
	}

	resp := managementCall(t, "/aliases/set", map[string][]string{
		"data": {`[{"name":"live-model","alias":"GROK-4.7-XHIGH-FAST"}]`},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
	}
	var failed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "error" || failed.Error != "第 1 行：别名 GROK-4.7-XHIGH-FAST 与已有模型 ID 冲突" {
		t.Fatalf("error body = %+v", failed)
	}
	path, err := aliasFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected save wrote the file: %v", err)
	}

	// A non-clashing alias still saves.
	resp = managementCall(t, "/aliases/set", map[string][]string{
		"data": {`[{"name":"live-model","alias":"my-live"}]`},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("clean save status = %d, body %s", resp.StatusCode, resp.Body)
	}
	if got := loadCursorAliases(); len(got) != 1 || got[0].Alias != "my-live" {
		t.Fatalf("clean save aliases = %+v", got)
	}
}
