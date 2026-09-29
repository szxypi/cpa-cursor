package main

import (
	"encoding/binary"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	maxAgentDiagnosticSuccesses = 5
	maxAgentDiagnosticFailures  = 20
)

type agentDiagnosticBudget struct {
	successes atomic.Uint32
	failures  atomic.Uint32
}

type agentDiagnosticConfig struct {
	enabled bool
	model   string
	budget  *agentDiagnosticBudget
}

var (
	agentDiagnosticSettings atomic.Pointer[agentDiagnosticConfig]
	agentDiagnosticTotal    atomic.Uint32
)

func setAgentDiagnostics(enabled bool, model string) {
	model = strings.TrimSpace(model)
	budget := &agentDiagnosticBudget{}
	if previous := agentDiagnosticSettings.Load(); previous != nil && previous.model == model && previous.budget != nil {
		budget = previous.budget
	}
	agentDiagnosticSettings.Store(&agentDiagnosticConfig{enabled: enabled, model: model, budget: budget})
}

func traceAgentModel(model string) bool {
	cfg := agentDiagnosticSettings.Load()
	return cfg != nil && cfg.enabled && cfg.model != "" && cfg.model == model
}

type agentTrace struct {
	Enabled            bool
	Frames             int
	BufferedFrames     int
	Compressed         int
	DecompressFailures int
	Trailers           int
	TrailerCode        string
	TextEvents         int
	FinishEvents       int
	ContextRequests    int
	OtherEvents        int
	UnknownFrames      int
	MalformedFrames    int
	EOF                bool
	Pending            bool
	ReadError          bool
}

func agentTraceBucket(count int) string {
	switch {
	case count == 0:
		return "0"
	case count == 1:
		return "1"
	case count <= 3:
		return "2-3"
	case count <= 7:
		return "4-7"
	default:
		return "8+"
	}
}

func agentTrailerCode(payload []byte, flags byte) string {
	if flags&0x01 != 0 {
		return "compressed"
	}
	if len(payload) > 1024 {
		return "oversized"
	}
	return parseAgentTrailerCode(payload)
}

func parseAgentTrailerCode(payload []byte) string {
	var trailer struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &trailer); err != nil {
		return "undecodable"
	}
	if trailer.Error == nil {
		return "ok"
	}
	switch trailer.Error.Code {
	case "resource_exhausted", "unauthenticated", "permission_denied", "unavailable", "internal", "invalid_argument":
		return trailer.Error.Code
	default:
		return "other_error"
	}
}

// observeBufferedAgentTrailers inspects only complete frames already read past
// the finish event. It never consumes more upstream data or changes the result.
func observeBufferedAgentTrailers(pending []byte, trace *agentTrace) {
	for len(pending) >= 5 {
		length := int(binary.BigEndian.Uint32(pending[1:5]))
		if length > len(pending)-5 {
			return
		}
		flags := pending[0]
		payload := pending[5 : 5+length]
		trace.Frames++
		trace.BufferedFrames++
		if flags&0x01 != 0 {
			trace.Compressed++
		}
		if flags&0x02 != 0 {
			trace.Trailers++
			trace.TrailerCode = agentTrailerCode(payload, flags)
		}
		pending = pending[5+length:]
	}
}

func formatAgentTrace(result agentRunResult) string {
	trace := result.Trace
	exit := "aborted"
	if trace.ReadError {
		exit = "read_error"
	} else if trace.FinishEvents > 0 {
		exit = "finished_event"
	} else if trace.EOF {
		exit = "eof"
	}
	trailer := trace.TrailerCode
	if trace.Trailers == 0 {
		trailer = "not_observed"
	}
	outcome := "text"
	if result.Fatal != "" {
		outcome = "error"
	} else if strings.TrimSpace(result.Text) == "" {
		outcome = "empty"
	}
	data := struct {
		Schema             int    `json:"schema"`
		Outcome            string `json:"outcome"`
		Exit               string `json:"exit"`
		Frames             string `json:"frames"`
		BufferedFrames     string `json:"buffered_frames"`
		Compressed         string `json:"compressed"`
		DecompressFailures string `json:"decompress_failures"`
		TextEvents         string `json:"text_events"`
		FinishEvents       string `json:"finish_events"`
		ContextRequests    string `json:"context_requests"`
		OtherEvents        string `json:"other_events"`
		UnknownFrames      string `json:"unknown_frames"`
		MalformedFrames    string `json:"malformed_frames"`
		Pending            bool   `json:"pending"`
		Trailer            string `json:"trailer"`
	}{
		Schema: 1, Outcome: outcome, Exit: exit,
		Frames: agentTraceBucket(trace.Frames), BufferedFrames: agentTraceBucket(trace.BufferedFrames),
		Compressed:         agentTraceBucket(trace.Compressed),
		DecompressFailures: agentTraceBucket(trace.DecompressFailures),
		TextEvents:         agentTraceBucket(trace.TextEvents), FinishEvents: agentTraceBucket(trace.FinishEvents),
		ContextRequests: agentTraceBucket(trace.ContextRequests), OtherEvents: agentTraceBucket(trace.OtherEvents),
		UnknownFrames: agentTraceBucket(trace.UnknownFrames), MalformedFrames: agentTraceBucket(trace.MalformedFrames),
		Pending: trace.Pending, Trailer: trailer,
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

func recordAgentTrace(model string, result agentRunResult) {
	cfg := agentDiagnosticSettings.Load()
	if !result.Trace.Enabled || cfg == nil || !cfg.enabled || cfg.model != model || cfg.budget == nil {
		return
	}
	if result.Fatal == "" && strings.TrimSpace(result.Text) != "" {
		if cfg.budget.successes.Add(1) > maxAgentDiagnosticSuccesses {
			return
		}
	} else if cfg.budget.failures.Add(1) > maxAgentDiagnosticFailures {
		return
	}
	if agentDiagnosticTotal.Add(1) > 50 {
		return
	}
	current := agentDiagnosticSettings.Load()
	if current == nil || !current.enabled || current.model != model {
		return
	}
	hostLog("info", "agent_diagnostics model="+strconv.Quote(model)+" "+formatAgentTrace(result))
}
