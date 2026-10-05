package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Resource paths exposed under /v0/resource/plugins/cpa-cursor/*.
const (
	panelPath     = "/panel"
	accountsPath  = "/accounts"
	importPath    = "/import"
	modelsPath    = "/models"
	modelsSetPath = "/models/set"
	testPath      = "/test"
)

func handleManagementRegister() ([]byte, error) {
	return okEnvelope(struct {
		Resources []pluginapi.ResourceRoute `json:"resources,omitempty"`
	}{
		Resources: []pluginapi.ResourceRoute{
			{Path: panelPath, Menu: "Cursor 账号", Description: "管理 Cursor 账号、导入 Token 与查看可用模型"},
			{Path: accountsPath, Description: "列出已导入的 Cursor 账号"},
			{Path: importPath, Description: "导入 Cursor access token 并保存凭证文件"},
			{Path: modelsPath, Description: "查询指定 Cursor 账号的可用模型列表"},
			{Path: modelsSetPath, Description: "设置选中的模型集合（ids=逗号分隔；reset=1 恢复全部启用）"},
			{Path: testPath, Description: "对 Cursor 上游进行连通性测试"},
		},
	})
}

func handleManagementRequest(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	switch {
	case strings.HasSuffix(req.Path, panelPath):
		return htmlResponse(panelHTML)
	case strings.HasSuffix(req.Path, accountsPath):
		return managementAccounts()
	case strings.HasSuffix(req.Path, importPath):
		return managementImportQuery(req.Query.Get("token"), req.Query.Get("machine_id"), req.Query.Get("email"))
	case strings.HasSuffix(req.Path, modelsPath):
		return managementModels(req.Query.Get("auth"), req.Query.Get("version"), req.Query.Get("client_type"))
	case strings.HasSuffix(req.Path, testPath):
		return managementTest(req.Query.Get("auth"), req.Query.Get("version"), req.Query.Get("client_type"))

	case strings.HasSuffix(req.Path, modelsSetPath):
		if req.Query.Get("reset") == "1" {
			if err := resetPolicy(); err != nil {
				return errorEnvelope("policy_reset_failed", err.Error()), nil
			}
			rewritten := rewriteCursorAuths()
			return jsonResponse(http.StatusOK, map[string]any{"status": "success", "reset": true, "refreshed": rewritten})
		}
		ids := strings.Split(req.Query.Get("ids"), ",")
		cleaned := make([]string, 0, len(ids))
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				cleaned = append(cleaned, id)
			}
		}
		if err := savePolicySelection(cleaned); err != nil {
			return errorEnvelope("policy_save_failed", err.Error()), nil
		}
		rewritten := rewriteCursorAuths()
		return jsonResponse(http.StatusOK, map[string]any{"status": "success", "selected": len(cleaned), "refreshed": rewritten})
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown resource path"})
	}
}

func htmlResponse(body string) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(body),
	})
}

func jsonResponse(status int, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       payload,
	})
}

// isCursorAuthEntry checks whether a host auth file belongs to cursor.
func isCursorAuthEntry(entry pluginapi.HostAuthFileEntry) bool {
	if entry.Provider == providerKey {
		return true
	}
	if strings.HasPrefix(entry.Name, "cursor-") {
		return true
	}
	return false
}

// hostAuthList reports every credential the host currently knows about.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHostAPI(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
	}
	return resp.Files, nil
}

// managementAccounts lists all imported cursor accounts. host.auth.list hides
// file credentials without a path attribute, so the panel scans the auth
// directory directly (the plugin runs inside the service process).
func managementAccounts() ([]byte, error) {
	dir := filepath.Join(cwdOrRoot(), "auths")
	matches, err := filepath.Glob(filepath.Join(dir, providerKey+"-*.json"))
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"accounts": []any{}, "error": err.Error()})
	}
	accounts := make([]map[string]any, 0, len(matches))
	for _, path := range matches {
		name := filepath.Base(path)
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		cred, errParse := parseCursorCredential(raw)
		if errParse != nil || cred.AccessToken == "" {
			continue
		}
		label := cred.Label
		if label == "" {
			label = cred.Email
		}
		if label == "" {
			label = name
		}
		mod := ""
		if info, errInfo := os.Stat(path); errInfo == nil {
			mod = info.ModTime().Format("2006-01-02 15:04")
		}
		masked := cred.AccessToken
		if len(masked) > 14 {
			masked = masked[:10] + "…" + masked[len(masked)-4:]
		}
		accounts = append(accounts, map[string]any{
			"name":    name,
			"label":   label,
			"email":   cred.Email,
			"token":   masked,
			"machine": cred.MachineID,
			"updated": mod,
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{"accounts": accounts})
}

// managementImportQuery handles GET /import?token=... — the host only
// dispatches GET to plugin resource routes, and the iframe cannot reach the
// management-authenticated POST channel, so import travels as a query.
func managementImportQuery(token, machineID, email string) ([]byte, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "AccessToken 不能为空"})
	}
	cred := newCursorCredential(token, strings.TrimSpace(machineID), strings.TrimSpace(email))
	storage, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	saved, err := hostAuthSave(cred.fileName(), storage)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	hostLog("info", "management imported "+cred.label()+" -> "+saved.Name)
	return jsonResponse(http.StatusOK, map[string]any{
		"status": "success",
		"label":  cred.label(),
		"file":   saved.Name,
	})
}

// hostAuthGet resolves one credential by auth index.
func hostAuthGet(authIndex string) ([]byte, error) {
	payload, err := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	raw, err := callHostAPI(pluginabi.MethodHostAuthGet, payload)
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// accountCredential resolves an auth file name to its entry and credential.
func accountCredential(authID string) (pluginapi.HostAuthFileEntry, *cursorCredential, error) {
	entries, err := hostAuthList()
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, nil, err
	}
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Name), authID) {
			continue
		}
		stored, err := hostAuthGet(entry.AuthIndex)
		if err != nil {
			return entry, nil, err
		}
		cred, err := parseCursorCredential(stored)
		if err != nil {
			return entry, nil, err
		}
		return entry, cred, nil
	}
	return pluginapi.HostAuthFileEntry{}, nil, fmt.Errorf("找不到认证文件 %s", authID)
}

// managementModels fetches the dynamic model catalog for an account.
func managementModels(authName, versionOverride, clientTypeOverride string) ([]byte, error) {
	authName = strings.TrimSpace(authName)
	if authName == "" {
		return jsonResponse(http.StatusOK, map[string]any{
			"models": staticCursorModels,
			"source": "static",
		})
	}
	entry, cred, err := accountCredential(authName)
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"models": staticCursorModels,
			"source": "static",
			"error":  err.Error(),
		})
	}
	infos := fetchModelsWithOverrides(cred, versionOverride, clientTypeOverride)
	// List the effort families too, so that the selection can enable them.
	infos = append(infos, cursorFamilyInfos(deriveCursorFamilies(infos))...)
	set := selectedModelSet()
	rows := make([]map[string]any, 0, len(infos))
	for _, m := range infos {
		rows = append(rows, map[string]any{
			"id":      m.ID,
			"name":    m.DisplayName,
			"aliases": m.Version,
			"enabled": set == nil || set[m.ID],
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"models": rows,
		"source": "dynamic",
		"count":  len(rows),
		"auth":   entry.Name,
		"probe":  probeLastError,
		"mode":   map[bool]string{true: "explicit", false: "all"}[set != nil],
	})
}

// managementTest probes Cursor upstream connectivity with the real token when
// an account is named, otherwise an unauthenticated reachability probe.
// Optional version/client_type query params override the client headers for
// one-shot comparisons (which model catalog a client build sees).
func managementTest(authName, versionOverride, clientTypeOverride string) ([]byte, error) {
	authName = strings.TrimSpace(authName)
	identity := cursorIdentity{AccessToken: "probe", GhostMode: true}
	authLabel := ""
	if authName != "" {
		_, cred, err := accountCredential(authName)
		if err == nil {
			identity = cred.identity()
			authLabel = authName
		}
	}

	start := time.Now()
	headers := buildCursorHeaders(identity)
	if versionOverride != "" {
		headers["x-cursor-client-version"] = versionOverride
	}
	if clientTypeOverride != "" {
		headers["x-cursor-client-type"] = clientTypeOverride
	}
	headers["Content-Type"] = "application/proto"
	headers["User-Agent"] = "connectrpc/1.x-go"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	_, err := fetchUsableModels(ctx, cursorAgentBase+cursorModelsPath, headers)
	latency := time.Since(start).Milliseconds()

	status := "ok"
	message := fmt.Sprintf("上游连接正常（%d ms）", latency)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthenticated") {
			message = fmt.Sprintf("上游可达，返回未认证（%d ms）", latency)
		} else {
			status = "error"
			message = fmt.Sprintf("连接失败（%d ms）：%s", latency, errStr)
		}
	}

	return jsonResponse(http.StatusOK, map[string]any{
		"status":  status,
		"latency": latency,
		"message": message,
		"auth":    authLabel,
	})
}

// rewriteCursorAuths rewrites every cursor credential file unchanged so the
// host watcher reloads them and re-pulls model.for_auth — RegisterClient has
// replace semantics, so the shrunken selection takes effect immediately.
// host.auth.list hides file entries without a path attribute, so this walks
// the auth directory directly (the plugin runs inside the service process).
func rewriteCursorAuths() int {
	dir := filepath.Join(cwdOrRoot(), "auths")
	matches, err := filepath.Glob(filepath.Join(dir, providerKey+"-*.json"))
	if err != nil || len(matches) == 0 {
		hostLog("warn", fmt.Sprintf("rewrite found no cursor auths in %s: %v", dir, err))
		return 0
	}
	count := 0
	for _, path := range matches {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			hostLog("warn", "rewrite read failed "+path+": "+errRead.Error())
			continue
		}
		if _, err := parseCursorCredential(raw); err != nil {
			continue
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			hostLog("warn", "rewrite write failed "+path+": "+err.Error())
			continue
		}
		count++
	}
	hostLog("info", fmt.Sprintf("rewrite done: %d auth files", count))
	return count
}

func cwdOrRoot() string {
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return cwd
	}
	return "/"
}

// probeLastError records the last raw probe outcome for panel diagnostics.
var probeLastError string

// fetchModelsWithOverrides queries GetUsableModels with optional one-shot
// client header overrides (for comparing what different client builds see).
func fetchModelsWithOverrides(cred *cursorCredential, versionOverride, clientTypeOverride string) []pluginapi.ModelInfo {
	headers := buildCursorHeaders(cred.identity())
	if versionOverride != "" {
		headers["x-cursor-client-version"] = versionOverride
	}
	if clientTypeOverride != "" {
		headers["x-cursor-client-type"] = clientTypeOverride
	}
	headers["Content-Type"] = "application/proto"
	headers["User-Agent"] = "connectrpc/1.x-go"
	delete(headers, "Connect-Accept-Encoding")
	delete(headers, "Connect-Protocol-Version")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	body, err := fetchUsableModels(ctx, cursorAgentBase+cursorModelsPath, headers)
	probeLastError = fmt.Sprintf("%d bytes: %v", len(body), err)
	if err != nil {
		hostLog("warn", "models probe failed: "+err.Error())
		return staticCursorModels
	}
	var out []pluginapi.ModelInfo
	for _, m := range parseUsableModels(body) {
		out = append(out, pluginapi.ModelInfo{ID: m.ID, Name: m.ID, DisplayName: m.Name, Version: strings.Join(m.Aliases, ", ")})
	}
	if len(out) == 0 {
		return staticCursorModels
	}
	return out
}
