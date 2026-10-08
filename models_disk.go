package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The live account catalog is saved per token so a cold start behind a dead
// proxy still registers the account's models and effort families. The file
// name is a token hash: it must not leak the token itself.

// cursorCatalogDefaultDir mirrors the checkpoint store's location under the
// service working directory.
func cursorCatalogDefaultDir() string {
	return filepath.Join(cwdOrRoot(), "cpa-cursor", "catalog")
}

// cursorCatalogDiskEntry is one model of the saved live catalog.
type cursorCatalogDiskEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

func cursorCatalogDiskPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(cursorCatalogDefaultDir(), hex.EncodeToString(sum[:])+".json")
}

// saveCursorCatalogDisk writes the live catalog atomically; a failure only
// costs the next cold start its fallback.
func saveCursorCatalogDisk(key string, models []pluginapi.ModelInfo) {
	entries := make([]cursorCatalogDiskEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, cursorCatalogDiskEntry{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	dir := cursorCatalogDefaultDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), cursorCatalogDiskPath(key)) != nil {
		os.Remove(tmp.Name())
	}
}

// loadCursorCatalogDisk reads the last successfully saved live catalog. A
// missing or unreadable file means "no fallback", not an error.
func loadCursorCatalogDisk(key string) []pluginapi.ModelInfo {
	raw, err := os.ReadFile(cursorCatalogDiskPath(key))
	if err != nil {
		return nil
	}
	var entries []cursorCatalogDiskEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, pluginapi.ModelInfo{ID: e.ID, Name: e.Name, DisplayName: e.DisplayName})
	}
	return out
}
