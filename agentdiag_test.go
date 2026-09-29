package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestConfigureAgentDiagnostics(t *testing.T) {
	previous := agentDiagnosticSettings.Load()
	t.Cleanup(func() { agentDiagnosticSettings.Store(previous) })

	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("enabled: true\nagent_diagnostics: true\nagent_diagnostics_model: grok-4.7-xhigh-fast\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(request); err != nil {
		t.Fatal(err)
	}
	if !traceAgentModel("grok-4.7-xhigh-fast") || traceAgentModel("composer-2.5-fast") {
		t.Fatal("diagnostics must be restricted to the configured model")
	}
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	if traceAgentModel("grok-4.7-xhigh-fast") {
		t.Fatal("diagnostics must be disabled by default")
	}
}

func TestAgentTrailerCodeIsAllowlisted(t *testing.T) {
	for _, tt := range []struct {
		name  string
		body  []byte
		flags byte
		want  string
	}{
		{"success", []byte(`{}`), 0x02, "ok"},
		{"quota", []byte(`{"error":{"code":"resource_exhausted","message":"private text"}}`), 0x02, "resource_exhausted"},
		{"unknown", []byte(`{"error":{"code":"private text"}}`), 0x02, "other_error"},
		{"malformed", []byte(`not JSON`), 0x02, "undecodable"},
		{"compressed", []byte(`secret`), 0x03, "compressed"},
		{"oversized", bytes.Repeat([]byte{'x'}, 1025), 0x02, "oversized"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentTrailerCode(tt.body, tt.flags); got != tt.want {
				t.Fatalf("code = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentTraceObservesFramesWithoutLoggingContent(t *testing.T) {
	const sensitive = "private user response"
	text := wrapConnectFrame(fieldBytes(1, fieldBytes(1, fieldString(1, sensitive))))
	trailer := wrapConnectFrame([]byte(`{"error":{"code":"resource_exhausted","message":"private error"}}`))
	trailer[0] = 0x02

	client := &agentClient{}
	result := agentRunResult{Trace: agentTrace{Enabled: true}}
	pending := client.consumeAgentFrames(text[:len(text)-1], &result, nil)
	if result.Text != "" {
		t.Fatal("a partial frame must not emit text")
	}
	pending = client.consumeAgentFrames(append(pending, append(text[len(text)-1:], trailer...)...), &result, nil)
	if len(pending) != 0 || result.Text != sensitive || result.Trace.Frames != 2 || result.Trace.Trailers != 1 {
		t.Fatalf("unexpected frame state: remaining=%d frames=%d trailers=%d", len(pending), result.Trace.Frames, result.Trace.Trailers)
	}
	formatted := formatAgentTrace(result)
	if strings.Contains(formatted, sensitive) || strings.Contains(formatted, "private error") || !strings.Contains(formatted, `"trailer":"resource_exhausted"`) {
		t.Fatalf("unsafe diagnostic output: %s", formatted)
	}
	if result.Fatal != "cursor AgentService resource_exhausted" {
		t.Fatalf("trailer errors must be surfaced: %s", result.Fatal)
	}
	if got := classifyAgentFailure("cursor AgentService invalid_argument"); got.code != "invalid_request_error" || got.status != http.StatusBadRequest {
		t.Fatalf("invalid_argument must be a request fault: %+v", got)
	}
}

func TestAgentTraceObservesMalformedAndCompressedFrames(t *testing.T) {
	compressed := wrapConnectFrame([]byte("not gzip"))
	compressed[0] = 0x01
	malformed := wrapConnectFrame([]byte{0xff})
	unknown := wrapConnectFrame(fieldVarint(9, 1))

	client := &agentClient{}
	result := agentRunResult{Trace: agentTrace{Enabled: true}}
	client.consumeAgentFrames(append(append(compressed, malformed...), unknown...), &result, nil)
	if result.Trace.Frames != 3 || result.Trace.Compressed != 1 || result.Trace.DecompressFailures != 1 || result.Trace.MalformedFrames != 1 || result.Trace.UnknownFrames != 1 {
		t.Fatalf("incorrect frame classifications: %+v", result.Trace)
	}
}

func TestAgentTraceDistinguishesEOFAndFinishEvent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go io.Copy(io.Discard, reader)

	text := wrapConnectFrame(fieldBytes(1, fieldBytes(1, fieldString(1, "ok"))))
	client := &agentClient{pw: writer, resp: &http.Response{Body: io.NopCloser(bytes.NewReader(text))}}
	result := client.runTurn([]byte("request"), nil, true)
	if result.Fatal != "" || result.Text != "ok" || !result.Finished || !result.Trace.EOF || result.Trace.FinishEvents != 0 {
		t.Fatalf("EOF must remain distinct from a finish event: %+v", result)
	}
	if !strings.Contains(formatAgentTrace(result), `"exit":"eof"`) {
		t.Fatal("diagnostic exit must report EOF")
	}
}

func TestAgentTraceObservesBufferedTrailerAfterFinish(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go io.Copy(io.Discard, reader)

	finish := wrapConnectFrame(fieldBytes(1, fieldBytes(14, nil)))
	trailer := wrapConnectFrame([]byte(`{"error":{"code":"resource_exhausted","message":"secret"}}`))
	trailer[0] = 0x02
	client := &agentClient{pw: writer, resp: &http.Response{Body: io.NopCloser(bytes.NewReader(append(finish, trailer...)))}}
	result := client.runTurn([]byte("request"), nil, true)
	if !result.Finished || result.Trace.FinishEvents != 1 || result.Trace.Trailers != 1 || result.Trace.BufferedFrames != 0 || result.Trace.TrailerCode != "resource_exhausted" {
		t.Fatalf("finish and buffered trailer must be distinct: %+v", result.Trace)
	}
	if result.Fatal != "cursor AgentService resource_exhausted" || !result.Ended {
		t.Fatal("error trailer after finish must override completion")
	}
}

func TestAgentDiagnosticBudgetFollowsConfiguredModel(t *testing.T) {
	previous := agentDiagnosticSettings.Load()
	t.Cleanup(func() { agentDiagnosticSettings.Store(previous) })

	setAgentDiagnostics(true, "model-a")
	initial := agentDiagnosticSettings.Load().budget
	initial.successes.Store(maxAgentDiagnosticSuccesses)
	setAgentDiagnostics(true, "model-a")
	if agentDiagnosticSettings.Load().budget != initial {
		t.Fatal("reloading the same model must preserve its diagnostic budget")
	}
	setAgentDiagnostics(true, "model-b")
	if !traceAgentModel("model-b") || traceAgentModel("model-a") || agentDiagnosticSettings.Load().budget.successes.Load() != 0 {
		t.Fatal("switching models must start a fresh budget for the new model")
	}
	setAgentDiagnostics(false, "model-b")
	if traceAgentModel("model-b") {
		t.Fatal("hot disabling diagnostics must take effect")
	}
}

func TestAgentTraceDoesNotLabelFatalAsFinished(t *testing.T) {
	formatted := formatAgentTrace(agentRunResult{Fatal: "private error", Trace: agentTrace{Enabled: true}})
	if !strings.Contains(formatted, `"exit":"aborted"`) || strings.Contains(formatted, "private error") {
		t.Fatalf("fatal exit must be sanitized: %s", formatted)
	}
}
