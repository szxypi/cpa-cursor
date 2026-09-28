package main

// Selected-model policy, persisted as cursor-models.yaml in the service
// working directory (same pattern as cpa-qoder's qoder.yaml): a missing file
// means "every model enabled"; the panel writes the explicit selection.

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const defaultPolicyFileName = "cursor-models.yaml"

type cursorPolicyFile struct {
	Selected []string `yaml:"selected_models"`
}

var (
	policyMu      sync.Mutex
	policyCache   *cursorPolicyFile
	policyPath    string
	policyModTime time.Time
)

func policyFilePath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, defaultPolicyFileName), nil
}

// loadPolicy re-reads cursor-models.yaml when it changed; parse errors keep
// the last good copy so a typo cannot take every model offline.
func loadPolicy() *cursorPolicyFile {
	path, err := policyFilePath()
	if err != nil {
		return &cursorPolicyFile{}
	}
	policyMu.Lock()
	defer policyMu.Unlock()
	info, statErr := os.Stat(path)
	if statErr != nil {
		policyCache = &cursorPolicyFile{}
		policyPath = path
		policyModTime = time.Time{}
		return policyCache
	}
	if policyCache != nil && policyPath == path && info.ModTime().Equal(policyModTime) {
		return policyCache
	}
	raw, readErr := os.ReadFile(path)
	parsed := &cursorPolicyFile{}
	if readErr == nil {
		if err := yaml.Unmarshal(raw, parsed); err != nil {
			hostLog("warn", "ignoring invalid "+path+": "+err.Error())
			parsed = &cursorPolicyFile{}
		}
	} else {
		hostLog("warn", "cannot read "+path+": "+readErr.Error())
	}
	policyCache = parsed
	policyPath = path
	policyModTime = info.ModTime()
	return parsed
}

// selectedModelSet returns nil when everything is enabled (no policy file),
// otherwise the explicit selection.
func selectedModelSet() map[string]bool {
	policy := loadPolicy()
	if policy == nil || len(policy.Selected) == 0 {
		return nil
	}
	set := make(map[string]bool, len(policy.Selected))
	for _, id := range policy.Selected {
		set[id] = true
	}
	return set
}

// savePolicySelection persists the selected ids; empty list removes the file
// (back to "everything enabled" would be indistinguishable from an empty
// selection, so an empty selection is stored explicitly).
func savePolicySelection(ids []string) error {
	path, err := policyFilePath()
	if err != nil {
		return err
	}
	payload, err := yaml.Marshal(&cursorPolicyFile{Selected: ids})
	if err != nil {
		return err
	}
	policyMu.Lock()
	defer policyMu.Unlock()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return err
	}
	info, statErr := os.Stat(path)
	policyCache = &cursorPolicyFile{Selected: ids}
	policyPath = path
	if statErr == nil {
		policyModTime = info.ModTime()
	}
	return nil
}

// resetPolicy removes the selection file (back to all models enabled).
func resetPolicy() error {
	path, err := policyFilePath()
	if err != nil {
		return err
	}
	policyMu.Lock()
	defer policyMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	policyCache = nil
	policyModTime = time.Time{}
	return nil
}

// policyAllows reports whether one model id is currently selected.
func policyAllows(id string) bool {
	set := selectedModelSet()
	return set == nil || set[id]
}

// filterByPolicy keeps only selected models; nil selection keeps everything.
func filterByPolicy(infos []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	set := selectedModelSet()
	if set == nil {
		return infos
	}
	var out []pluginapi.ModelInfo
	for _, m := range infos {
		if set[m.ID] {
			out = append(out, m)
		}
	}
	return out
}
