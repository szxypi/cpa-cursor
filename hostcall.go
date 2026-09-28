package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var errAllocateHostRequest = errors.New("allocate host callback request buffer")

// hostEnvelope mirrors pluginabi.Envelope's wire shape for decoding responses
// coming back from callHostAPI.
type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *hostError      `json:"error,omitempty"`
}

type hostError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func decodeHostEnvelope(method string, raw []byte, callCode int) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, callCode)
	}
	var env hostEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host callback %s failed: %s (%s)", method, strings.TrimSpace(env.Error.Message), env.Error.Code)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, callCode)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// hostLogRequest mirrors internal/pluginhost's rpcHostLogRequest wire shape.
type hostLogRequest struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// hostLog writes one line to the host journal. CPA's formatter does not render
// structured fields, so callers fold everything worth keeping into message.
func hostLog(level, message string) {
	payload, err := json.Marshal(hostLogRequest{Level: level, Message: logPrefix + message})
	if err != nil {
		return
	}
	// Logging runs from background goroutines with nobody to report to.
	_, _ = callHostAPI(pluginabi.MethodHostLog, payload)
}

// rpcHostHTTPRequest mirrors internal/pluginhost's rpcHostHTTPRequest.
type rpcHostHTTPRequest struct {
	HostCallbackID string                     `json:"host_callback_id,omitempty"`
	Method         string                     `json:"method,omitempty"`
	URL            string                     `json:"url,omitempty"`
	Headers        map[string][]string        `json:"headers,omitempty"`
	Body           []byte                     `json:"body,omitempty"`
	WireProfile    *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

// rpcHostHTTPStreamResponse mirrors internal/pluginhost's response for
// host.http.do_stream.
type rpcHostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// cursorWireProfile keeps the host transport from negotiating compression:
// Cursor's ConnectRPC endpoint rejects compressed request frames, so the
// protobuf body must arrive uncompressed.
func cursorWireProfile() *pluginapi.HTTPWireProfile {
	return &pluginapi.HTTPWireProfile{DisableAutoCompression: true}
}

func headerMap(headers map[string]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string][]string, len(headers))
	for k, v := range headers {
		out[k] = []string{v}
	}
	return out
}

// hostHTTPDo performs a non-streaming upstream call through the host transport
// so request-log capture and proxy policy still apply.
func hostHTTPDo(callbackID, method, url string, headers map[string]string, body []byte) (*pluginapi.HTTPResponse, error) {
	payload, err := json.Marshal(rpcHostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         method,
		URL:            url,
		Headers:        headerMap(headers),
		Body:           body,
		WireProfile:    cursorWireProfile(),
	})
	if err != nil {
		return nil, err
	}
	raw, err := callHostAPI(pluginabi.MethodHostHTTPDo, payload)
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	return &resp, nil
}

// hostHTTPDoStream opens an upstream streaming call and returns its status plus
// the host-side stream handle used by hostHTTPStreamRead.
func hostHTTPDoStream(callbackID, method, url string, headers map[string]string, body []byte) (*rpcHostHTTPStreamResponse, error) {
	payload, err := json.Marshal(rpcHostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         method,
		URL:            url,
		Headers:        headerMap(headers),
		Body:           body,
		WireProfile:    cursorWireProfile(),
	})
	if err != nil {
		return nil, err
	}
	raw, err := callHostAPI(pluginabi.MethodHostHTTPDoStream, payload)
	if err != nil {
		return nil, err
	}
	var resp rpcHostHTTPStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	return &resp, nil
}

func hostHTTPStreamRead(streamID string) (*rpcHostHTTPStreamReadResponse, error) {
	payload, err := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: streamID})
	if err != nil {
		return nil, err
	}
	raw, err := callHostAPI(pluginabi.MethodHostHTTPStreamRead, payload)
	if err != nil {
		return nil, err
	}
	var resp rpcHostHTTPStreamReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.stream_read response: %w", err)
	}
	return &resp, nil
}

func hostHTTPStreamClose(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload, err := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: streamID})
	if err != nil {
		return
	}
	_, _ = callHostAPI(pluginabi.MethodHostHTTPStreamClose, payload)
}

// hostStreamEmit pushes one chunk to the client stream opened by
// executor.execute_stream.
func hostStreamEmit(streamID string, chunk []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("cursor: plugin stream id is required")
	}
	payload, err := json.Marshal(rpcStreamEmitRequest{StreamID: streamID, Payload: chunk})
	if err != nil {
		return err
	}
	_, err = callHostAPI(pluginabi.MethodHostStreamEmit, payload)
	return err
}

// hostStreamClose ends the client stream, optionally with an error.
func hostStreamClose(streamID, errMessage string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload, err := json.Marshal(rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMessage)})
	if err != nil {
		return
	}
	_, _ = callHostAPI(pluginabi.MethodHostStreamClose, payload)
}

// hostAuthSave persists a credential file through the host so the watcher picks
// it up exactly like an OAuth login performed by CPA itself.
func hostAuthSave(name string, credential []byte) (*pluginapi.HostAuthSaveResponse, error) {
	payload, err := json.Marshal(pluginapi.HostAuthSaveRequest{Name: name, JSON: credential})
	if err != nil {
		return nil, err
	}
	raw, err := callHostAPI(pluginabi.MethodHostAuthSave, payload)
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostAuthSaveResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.save response: %w", err)
	}
	return &resp, nil
}

// statusFromHTTP maps an upstream status onto the plugin error envelope status
// CPA uses to classify failures for the client.
func statusFromHTTP(status int) int {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusTooManyRequests, status == http.StatusNotFound:
		return status
	case status >= 500:
		return status
	default:
		return http.StatusBadGateway
	}
}
