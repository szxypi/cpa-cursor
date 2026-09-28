package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Resource paths exposed under /v0/resource/plugins/cpa-cursor/*.
const (
	panelPath    = "/panel"
	accountsPath = "/accounts"
	importPath   = "/import"
	modelsPath   = "/models"
	testPath     = "/test"
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
		return managementImport(req.Body)
	case strings.HasSuffix(req.Path, modelsPath):
		return managementModels(req.Query.Get("auth"))
	case strings.HasSuffix(req.Path, testPath):
		return managementTest(req.Query.Get("auth"))
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

// managementAccounts lists all imported cursor accounts.
func managementAccounts() ([]byte, error) {
	entries, err := hostAuthList()
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"accounts": []any{}, "error": err.Error()})
	}
	accounts := make([]map[string]any, 0)
	for _, entry := range entries {
		if !isCursorAuthEntry(entry) {
			continue
		}
		label := entry.Label
		if label == "" {
			label = entry.Email
		}
		if label == "" {
			label = entry.Name
		}
		accounts = append(accounts, map[string]any{
			"name":        entry.Name,
			"label":       label,
			"status":      entry.Status,
			"disabled":    entry.Disabled,
			"unavailable": entry.Unavailable,
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{"accounts": accounts})
}

// importPayload is the POST body for /import.
type importPayload struct {
	Token     string `json:"token"`
	MachineID string `json:"machine_id"`
	Email     string `json:"email"`
}

func managementImport(body []byte) ([]byte, error) {
	var payload importPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "无效的 JSON 数据"})
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "AccessToken 不能为空"})
	}
	cred := newCursorCredential(token, strings.TrimSpace(payload.MachineID), strings.TrimSpace(payload.Email))
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
func managementModels(authName string) ([]byte, error) {
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
	models := catalogForToken(cred.identity())
	return jsonResponse(http.StatusOK, map[string]any{
		"models": models,
		"source": "dynamic",
		"count":  len(models),
		"auth":   entry.Name,
	})
}

// managementTest probes Cursor upstream connectivity with the real token when
// an account is named, otherwise an unauthenticated reachability probe.
func managementTest(authName string) ([]byte, error) {
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
