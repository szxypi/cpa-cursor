package main

// Cursor AgentService client over native HTTP/2.
//
// agent.api5.cursor.sh refuses HTTP/1.1, and the run RPC is a true duplex
// stream (the server asks for IDE context mid-flight via exec_request), which
// the host callback API cannot express — it has no stream_write. So this file
// dials directly with net/http, whose transport negotiates h2 via ALPN on its
// own. Trade-off: these calls bypass the host transport, so request-log
// capture and proxy policy do not apply to them. ChatService traffic (the
// agentic path) still goes through the host callbacks.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// agentClient wraps one duplex AgentService run.
type agentClient struct {
	client *http.Client
	pr     *io.PipeReader
	pw     *io.PipeWriter
	resp   *http.Response
	cancel context.CancelFunc
}

// openAgentStream starts a POST whose body stays open for mid-flight writes.
func openAgentStream(ctx context.Context, url string, headers map[string]string) (*agentClient, int, error) {
	pr, pw := io.Pipe()
	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, pr)
	if err != nil {
		cancel()
		pr.Close()
		pw.Close()
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// h2 request body without a known length; the server treats the stream as
	// half-open until we close it.
	req.ContentLength = -1

	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxConnsPerHost:     2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Do(req)
	if err != nil {
		cancel()
		pr.Close()
		pw.Close()
		transport.CloseIdleConnections()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		cancel()
		transport.CloseIdleConnections()
		return nil, resp.StatusCode, fmt.Errorf("cursor AgentService %d: %s", resp.StatusCode, string(body))
	}
	return &agentClient{client: client, pr: pr, pw: pw, resp: resp, cancel: cancel}, resp.StatusCode, nil
}

// write sends another client frame on the run stream.
func (a *agentClient) write(frame []byte) error {
	_, err := a.pw.Write(frame)
	return err
}

// close terminates the stream and the connection.
func (a *agentClient) close() {
	a.cancel()
	a.pr.CloseWithError(io.EOF)
	a.pw.CloseWithError(io.EOF)
	if a.resp != nil {
		a.resp.Body.Close()
	}
	if tr, ok := a.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// agentRunResult is what runAgentTurn reports back to the executor.
type agentRunResult struct {
	Text     string
	Finished bool
	// Fatal is a protocol-level failure: it must surface as an error, not as
	// assistant text.
	Fatal string
}

// runAgentTurn drives one AgentService run: sends the run frame, answers the
// RequestContext handshake, and streams text deltas to onDelta until the turn
// finishes. Remaining server frames after finish are drained but ignored.
func (a *agentClient) runTurn(runFrame []byte, onDelta func(string)) agentRunResult {
	if err := a.write(runFrame); err != nil {
		return agentRunResult{Fatal: fmt.Sprintf("cursor AgentService write failed: %v", err)}
	}

	reader := bufio.NewReader(a.resp.Body)
	var pending []byte
	result := agentRunResult{}

	for !result.Finished && result.Fatal == "" {
		chunk := make([]byte, 32*1024)
		n, err := reader.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)
			pending = a.consumeAgentFrames(pending, &result, onDelta)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if result.Finished {
				break
			}
			result.Fatal = fmt.Sprintf("cursor AgentService read failed: %v", err)
			break
		}
	}
	// Half-close so the server sees a clean end even if we stop early.
	a.pw.CloseWithError(io.EOF)
	if result.Fatal == "" && !result.Finished {
		result.Finished = true
	}
	return result
}

// consumeAgentFrames parses every complete frame in pending and returns the
// remainder.
func (a *agentClient) consumeAgentFrames(pending []byte, result *agentRunResult, onDelta func(string)) []byte {
	for len(pending) >= 5 {
		flags := pending[0]
		length := int(uint32(pending[1])<<24 | uint32(pending[2])<<16 | uint32(pending[3])<<8 | uint32(pending[4]))
		if len(pending) < 5+length {
			break
		}
		payload := pending[5 : 5+length]
		pending = pending[5+length:]

		if flags&0x02 != 0 { // trailer frame: end of stream metadata
			continue
		}
		if flags&0x01 != 0 { // gzip
			var err error
			payload, err = gunzip(payload)
			if err != nil {
				continue
			}
		}
		a.handleAgentPayload(payload, result, onDelta)
		if result.Finished || result.Fatal != "" {
			break
		}
	}
	return pending
}

func (a *agentClient) handleAgentPayload(payload []byte, result *agentRunResult, onDelta func(string)) {
	update, execRequest, execSupported := decodeAgentServerMessage(payload)
	if execRequest {
		if execSupported {
			// The server wants IDE context; acknowledge with an empty one.
			if err := a.write(createRequestContextResponse()); err != nil {
				result.Fatal = fmt.Sprintf("cursor AgentService context write failed: %v", err)
			}
		} else {
			// Every other exec variant is an editor-backed tool (shell, read,
			// write, …) this headless plugin cannot service; failing beats
			// narrating protocol state as assistant text.
			result.Fatal = "cursor AgentService requested an unsupported IDE tool"
		}
		return
	}
	if update.TextDelta != "" {
		result.Text += update.TextDelta
		if onDelta != nil {
			onDelta(update.TextDelta)
		}
	}
	if update.Finished {
		result.Finished = true
	}
}

// fetchUsableModels calls the unary GetUsableModels over h2 and returns the
// raw protobuf body.
func fetchUsableModels(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	defer transport.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("cursor GetUsableModels %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}
