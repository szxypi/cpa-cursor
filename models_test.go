package main

// Catalog merge, panel rewrite and disk-fallback tests. Every test works in a
// temporary working directory: the policy file, the auths directory and the
// saved catalog are all resolved from there.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetCursorCaches clears the package-level catalogs so tests do not see each
// other's results.
func resetCursorCaches() {
	cursorCatalogMu.Lock()
	cursorCatalogCache = map[string]cursorCatalogEntry{}
	cursorFamilyCache = map[string]map[string]cursorFamily{}
	cursorCatalogMu.Unlock()
	policyMu.Lock()
	policyCache = nil
	policyPath = ""
	policyModTime = time.Time{}
	policyMu.Unlock()
}

// stubLiveCatalog replaces the live pull for one test.
func stubLiveCatalog(t *testing.T, models []pluginapi.ModelInfo) {
	t.Helper()
	previous := fetchLiveCatalogFn
	fetchLiveCatalogFn = func(cursorIdentity) []pluginapi.ModelInfo { return models }
	t.Cleanup(func() { fetchLiveCatalogFn = previous })
}

func countModelID(models []pluginapi.ModelInfo, id string) int {
	n := 0
	for _, m := range models {
		if m.ID == id {
			n++
		}
	}
	return n
}

func hasModelID(models []pluginapi.ModelInfo, id string) bool {
	return countModelID(models, id) > 0
}

// TestCatalogForTokenDedupes covers a live catalog that carries an id the
// static list already has: the merged result must list it once.
func TestCatalogForTokenDedupes(t *testing.T) {
	t.Chdir(t.TempDir())
	resetCursorCaches()
	stubLiveCatalog(t, []pluginapi.ModelInfo{
		{ID: "grok-4.7-high", Name: "grok-4.7-high", DisplayName: "Grok 4.7 High"},
		{ID: "live-only-model", Name: "live-only-model", DisplayName: "Live Only"},
	})

	models := catalogForToken(cursorIdentity{AccessToken: "tok-dup"})
	if n := countModelID(models, "grok-4.7-high"); n != 1 {
		t.Fatalf("grok-4.7-high appears %d times, want 1", n)
	}
	if !hasModelID(models, "live-only-model") {
		t.Fatalf("live-only model missing from %v", models)
	}
	// The static entry keeps its description when the live entry has none.
	for _, m := range models {
		if m.ID == "grok-4.7-high" && m.Description == "" {
			t.Fatalf("static description was cleared: %+v", m)
		}
	}
}

// TestCatalogForTokenDiskFallback covers a dead live pull with a saved catalog:
// the result must carry the account's variants and the selected family.
func TestCatalogForTokenDiskFallback(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	resetCursorCaches()

	identity := cursorIdentity{AccessToken: "tok-disk"}
	key := cleanToken(identity.AccessToken)
	saveCursorCatalogDisk(key, []pluginapi.ModelInfo{
		{ID: "cursor-grok-4.6-high-fast", Name: "cursor-grok-4.6-high-fast", DisplayName: "Grok 4.6 High"},
		{ID: "cursor-grok-4.6-xhigh-fast", Name: "cursor-grok-4.6-xhigh-fast", DisplayName: "Grok 4.6 XHigh"},
	})
	stubLiveCatalog(t, nil)
	if err := savePolicySelection([]string{"cursor-grok-4.6-fast"}); err != nil {
		t.Fatal(err)
	}

	models := catalogForToken(identity)
	if !hasModelID(models, "cursor-grok-4.6-fast") {
		t.Fatalf("selected family missing from %v", models)
	}
	if hasModelID(models, "cursor-grok-4.6-high-fast") {
		t.Fatalf("unselected variant leaked into %v", models)
	}
	// The families stay cached for the executor's effort resolution.
	if len(cachedCursorFamilies(key)) == 0 {
		t.Fatal("family cache is empty after a disk fallback")
	}
}

// TestCatalogForTokenDiskFallbackTTL covers a dead live pull with no saved
// catalog: the static-only result is cached briefly, not for ten minutes.
func TestCatalogForTokenDiskFallbackTTL(t *testing.T) {
	t.Chdir(t.TempDir())
	resetCursorCaches()
	stubLiveCatalog(t, nil)

	identity := cursorIdentity{AccessToken: "tok-cold"}
	catalogForToken(identity)
	cursorCatalogMu.Lock()
	entry := cursorCatalogCache[cleanToken(identity.AccessToken)]
	cursorCatalogMu.Unlock()
	if entry.ttl != cursorCatalogFallbackTTL {
		t.Fatalf("cold-start ttl = %v, want %v", entry.ttl, cursorCatalogFallbackTTL)
	}

	// A successful live pull caches for the full TTL again.
	cursorCatalogMu.Lock()
	delete(cursorCatalogCache, cleanToken(identity.AccessToken))
	cursorCatalogMu.Unlock()
	stubLiveCatalog(t, []pluginapi.ModelInfo{{ID: "live-model", Name: "live-model"}})
	catalogForToken(identity)
	cursorCatalogMu.Lock()
	entry = cursorCatalogCache[cleanToken(identity.AccessToken)]
	cursorCatalogMu.Unlock()
	if entry.ttl != cursorCatalogTTL {
		t.Fatalf("live ttl = %v, want %v", entry.ttl, cursorCatalogTTL)
	}
}

// TestCatalogDiskPathHidesToken checks the saved catalog file name carries a
// hash, not the token.
func TestCatalogDiskPathHidesToken(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	token := "super-secret-token"
	saveCursorCatalogDisk(token, []pluginapi.ModelInfo{{ID: "m", Name: "m"}})
	path := cursorCatalogDiskPath(token)
	if strings.Contains(path, token) {
		t.Fatalf("path %q leaks the token", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("saved catalog not found: %v", err)
	}
}

// TestSaveSelectionClearsCatalogCache covers the panel save path: the merged
// catalog is dropped so the host re-pull sees the new selection.
func TestSaveSelectionClearsCatalogCache(t *testing.T) {
	t.Chdir(t.TempDir())
	resetCursorCaches()
	stubLiveCatalog(t, []pluginapi.ModelInfo{{ID: "live-model", Name: "live-model"}})

	identity := cursorIdentity{AccessToken: "tok-panel"}
	catalogForToken(identity)
	cursorCatalogMu.Lock()
	before := len(cursorCatalogCache)
	cursorCatalogMu.Unlock()
	if before == 0 {
		t.Fatal("cache was not primed")
	}

	request, err := json.Marshal(map[string]any{
		"Path":  "/models/set",
		"Query": map[string][]string{"ids": {"live-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleManagementRequest(request); err != nil {
		t.Fatal(err)
	}
	cursorCatalogMu.Lock()
	after := len(cursorCatalogCache)
	cursorCatalogMu.Unlock()
	if after != 0 {
		t.Fatalf("cache still holds %d entries after a selection save", after)
	}
}

// TestRewriteCursorAuthsRevision covers the panel rewrite: the file content
// must change, the credential must still parse to the same token, and unknown
// keys must survive.
func TestRewriteCursorAuthsRevision(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	authsDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(authsDir, "cursor-abcdef12.json")
	original := `{
  "type": "cursor",
  "access_token": "tok-rewrite",
  "machine_id": "machine-1",
  "GhostMode": true,
  "ghost_mode": true,
  "custom_future_key": "keep-me"
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if n := rewriteCursorAuths(); n != 1 {
		t.Fatalf("rewrite count = %d, want 1", n)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) == original {
		t.Fatal("file content did not change; the watcher would skip the reload")
	}
	cred, err := parseCursorCredential(updated)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "tok-rewrite" || cred.MachineID != "machine-1" {
		t.Fatalf("credential changed: %+v", cred)
	}
	if cred.GhostMode == nil || !*cred.GhostMode {
		t.Fatalf("ghost mode lost: %+v", cred.GhostMode)
	}

	var fields map[string]any
	if err := json.Unmarshal(updated, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "access_token", "machine_id", "GhostMode", "ghost_mode", "custom_future_key"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("key %q was dropped: %v", key, fields)
		}
	}
	if _, ok := fields["models_revision"]; !ok {
		t.Fatalf("models_revision missing: %v", fields)
	}

	// The host re-pulls models only when the parsed auth differs, so the
	// revision must reach what auth.parse returns, not just the file.
	before := parsedAuthData(t, []byte(original))
	after := parsedAuthData(t, updated)
	if string(before.StorageJSON) == string(after.StorageJSON) {
		t.Fatalf("parsed storage unchanged: %s", after.StorageJSON)
	}
	if after.Metadata["ModelsRevision"] == nil {
		t.Fatalf("parsed metadata has no revision: %v", after.Metadata)
	}
	time.Sleep(2 * time.Millisecond)
	if n := rewriteCursorAuths(); n != 1 {
		t.Fatalf("second rewrite count = %d, want 1", n)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(parsedAuthData(t, again).StorageJSON) == string(after.StorageJSON) {
		t.Fatal("second rewrite left the parsed storage unchanged")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// parsedAuthData runs auth.parse on raw and returns the auth the host keeps.
func parsedAuthData(t *testing.T, raw []byte) pluginapi.AuthData {
	t.Helper()
	request, err := json.Marshal(pluginapi.AuthParseRequest{FileName: "cursor-abcdef12.json", RawJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := handleAuthParse(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Result pluginapi.AuthParseResponse `json:"result"`
	}
	if err := json.Unmarshal(envelope, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Result.Handled {
		t.Fatalf("auth.parse did not handle the file: %s", envelope)
	}
	return decoded.Result.Auth
}

// TestCursorFamilyDisplayNameHasNoDoublePrefix covers an upstream id that
// already carries the cursor- prefix.
func TestCursorFamilyDisplayNameHasNoDoublePrefix(t *testing.T) {
	family := deriveCursorFamilies(catalogOf(
		"cursor-grok-4.6-high-fast", "cursor-grok-4.6-xhigh-fast",
	))["cursor-grok-4.6-fast"]
	if family.ID == "" {
		t.Fatal("family not derived")
	}
	info := family.modelInfo()
	if strings.Contains(info.DisplayName, "cursor-") {
		t.Fatalf("display name repeats the prefix: %q", info.DisplayName)
	}
	if want := "Cursor grok-4.6 fast (effort family)"; info.DisplayName != want {
		t.Fatalf("display name = %q, want %q", info.DisplayName, want)
	}
	// The family id itself keeps the prefix.
	if info.ID != "cursor-grok-4.6-fast" {
		t.Fatalf("family id = %q", info.ID)
	}

	plain := deriveCursorFamilies(catalogOf("grok-4.7-low", "grok-4.7-high"))["cursor-grok-4.7"].modelInfo()
	if want := "Cursor grok-4.7 (effort family)"; plain.DisplayName != want {
		t.Fatalf("plain display name = %q, want %q", plain.DisplayName, want)
	}
}
