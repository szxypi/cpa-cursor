package main

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// statusError is a plugin failure already classified with the HTTP status CPA
// should report. Omitting the status makes CPA degrade everything to 500,
// which clients read as a gateway fault and retry with backoff.
type statusError struct {
	status  int
	code    string
	message string
}

func newStatusError(status int, code, message string) *statusError {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &statusError{status: status, code: code, message: message}
}

func (e *statusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *statusError) envelope() []byte {
	if e == nil {
		return errorEnvelopeStatus("plugin_error", "unknown plugin error", http.StatusInternalServerError)
	}
	return errorEnvelopeStatus(e.code, e.message, e.status)
}

// asStatusError preserves an error's own classification instead of flattening
// every failure to a fixed fallback (status, code).
func asStatusError(err error, fallbackStatus int, fallbackCode string) *statusError {
	if err == nil {
		return nil
	}
	if se, ok := err.(*statusError); ok && se != nil {
		return se
	}
	return newStatusError(fallbackStatus, fallbackCode, err.Error())
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	return errorEnvelopeStatus(code, message, 0)
}

func errorEnvelopeStatus(code, message string, status int) []byte {
	raw, err := pluginabi.NewErrorEnvelope(code, message, status)
	if err != nil {
		// Marshalling a fixed struct cannot realistically fail; fall back to a
		// literal so the host still receives a well-formed envelope.
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"failed to encode error envelope"}}`)
	}
	return raw
}
