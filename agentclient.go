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
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// agentClient wraps one duplex AgentService run.
type agentClient struct {
	client         *http.Client
	pr             *io.PipeReader
	pw             *io.PipeWriter
	resp           *http.Response
	cancel         context.CancelFunc
	context        *agentContext
	onTool         func(*agentToolRequest) error
	writeMu        sync.Mutex
	toolRejections int
}

// openAgentStream starts a POST whose body stays open for mid-flight writes.
func openAgentStream(ctx context.Context, url string, headers map[string]string) (*agentClient, int, error) {
	pr, pw := io.Pipe()
	requestCtx, cancel := context.WithCancel(ctx)
	// 双向请求的 io.Pipe 在上下文取消时必须主动关闭，否则 h2 写协程
	// 仍等待下一帧，响应读取也可能无法结束。
	context.AfterFunc(requestCtx, func() {
		pr.CloseWithError(requestCtx.Err())
		pw.CloseWithError(requestCtx.Err())
	})
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
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err := a.pw.Write(frame)
	return err
}

// close terminates the stream and the connection.
func (a *agentClient) close() {
	if a == nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.pr != nil {
		a.pr.CloseWithError(io.EOF)
	}
	if a.pw != nil {
		a.pw.CloseWithError(io.EOF)
	}
	if a.resp != nil {
		a.resp.Body.Close()
	}
	if a.client != nil {
		if tr, ok := a.client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}

// agentRunResult is what runAgentTurn reports back to the executor.
type agentRunResult struct {
	Text        string
	Finished    bool
	Ended       bool
	Trace       agentTrace
	TrailerCode string
	// Fatal is a protocol-level failure: it must surface as an error, not as
	// assistant text.
	Fatal string
}

// runAgentTurn drives one AgentService run: sends the run frame, answers the
// RequestContext handshake, and streams text deltas to onDelta until the turn
// finishes. Remaining server frames after finish are drained but ignored.
func (a *agentClient) runTurn(runFrame []byte, onDelta func(string) error, diagnostic bool) agentRunResult {
	result := agentRunResult{Trace: agentTrace{Enabled: diagnostic}}
	if err := a.write(runFrame); err != nil {
		result.Fatal = fmt.Sprintf("cursor AgentService write failed: %v", err)
		return result
	}

	reader := bufio.NewReader(a.resp.Body)
	var pending []byte

	requestClosed := false
	for !result.Ended && result.Fatal == "" {
		chunk := make([]byte, 32*1024)
		n, err := reader.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)
			pending = a.consumeAgentFrames(pending, &result, onDelta)
		}
		if result.Finished && !requestClosed {
			// 完成事件不等于 RPC 成功；半关闭请求后读取最终 trailer。
			a.pw.Close()
			requestClosed = true
		}
		if err != nil {
			if err == io.EOF {
				result.Trace.EOF = true
				break
			}
			result.Trace.ReadError = true
			result.Fatal = fmt.Sprintf("cursor AgentService read failed: %v", err)
			break
		}
	}
	if diagnostic {
		observeBufferedAgentTrailers(pending, &result.Trace)
		result.Trace.Pending = len(pending) > 0
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
func decodeAgentTrailerCode(payload []byte, flags byte) string {
	if flags&1 != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return "malformed"
		}
		defer reader.Close()
		payload, err = io.ReadAll(io.LimitReader(reader, 65537))
		if err != nil {
			return "malformed"
		}
	}
	if len(payload) > 65536 {
		return "oversized"
	}
	return parseAgentTrailerCode(payload)
}

func (a *agentClient) consumeAgentFrames(pending []byte, result *agentRunResult, onDelta func(string) error) []byte {
	for len(pending) >= 5 {
		flags := pending[0]
		length := int(uint32(pending[1])<<24 | uint32(pending[2])<<16 | uint32(pending[3])<<8 | uint32(pending[4]))
		if len(pending) < 5+length {
			break
		}
		payload := pending[5 : 5+length]
		pending = pending[5+length:]
		if result.Trace.Enabled {
			result.Trace.Frames++
			if flags&0x01 != 0 {
				result.Trace.Compressed++
			}
		}

		if flags&0x02 != 0 { // trailer frame: end of stream metadata
			code := decodeAgentTrailerCode(payload, flags)
			result.TrailerCode = code
			if result.Trace.Enabled {
				result.Trace.Trailers++
				result.Trace.TrailerCode = code
			}
			if code != "ok" {
				result.Fatal = "cursor AgentService " + code
			}
			result.Ended = true
			break
		}
		if flags&0x01 != 0 { // gzip
			var err error
			payload, err = gunzip(payload)
			if err != nil {
				if result.Trace.Enabled {
					result.Trace.DecompressFailures++
				}
				continue
			}
		}
		a.handleAgentPayload(payload, result, onDelta)
		if result.Ended || result.Fatal != "" {
			break
		}
	}
	return pending
}

// agentPayloadTap 仅供 live 测试观察上游帧；生产环境保持 nil。
var agentPayloadTap func(payload []byte)

func (a *agentClient) handleAgentPayload(payload []byte, result *agentRunResult, onDelta func(string) error) {
	if agentPayloadTap != nil {
		agentPayloadTap(payload)
	}
	fields := decodeMessage(payload)
	if kv, ok := fieldFirst(fields, 4); ok && kv.IsLen {
		if a.context == nil {
			a.context = &agentContext{blobs: make(map[string][]byte)}
		}
		response, err := a.context.kvResponse(kv.Value)
		if err == nil {
			err = a.write(response)
		}
		if err != nil {
			result.Fatal = "cursor AgentService KV exchange failed"
			result.Finished = true
		}
		return
	}
	if query, ok := fieldFirst(fields, 7); ok && query.IsLen {
		response, handled := agentInteractionResponse(query.Value)
		if !handled {
			result.Fatal = "cursor invalid_request_error: unsupported Cursor interaction query"
			return
		}
		if err := a.write(response); err != nil {
			result.Fatal = fmt.Sprintf("cursor AgentService interaction write failed: %v", err)
		}
		return
	}
	update, execRequest, execSupported := decodeAgentServerMessage(payload)
	if execRequest {
		if result.Trace.Enabled {
			result.Trace.ContextRequests++
		}
		if execSupported {
			// 按 RequestContext 契约返回本次请求的系统规则。
			exec, _ := fieldFirst(fields, 2)
			if err := a.write(a.context.contextResponse(exec.Value)); err != nil {
				result.Fatal = fmt.Sprintf("cursor AgentService context write failed: %v", err)
			}
		} else if a.context != nil && a.context.tools != nil && a.onTool != nil {
			exec, _ := fieldFirst(fields, 2)
			tool, reply, err := a.context.tools.parseExec(exec.Value)
			if err == nil && len(reply) > 0 {
				a.toolRejections++
				if a.toolRejections > 16 {
					err = fmt.Errorf("too many unsupported native tools")
				} else {
					err = a.write(reply)
				}
			}
			if err == nil && tool != nil {
				err = a.onTool(tool)
			}
			if err != nil {
				result.Fatal = "cursor invalid_request_error: tool protocol unavailable: " + err.Error()
			}
		} else {
			result.Fatal = "cursor invalid_request_error: no client tool was declared for the requested IDE operation"
		}
		return
	}
	if result.Trace.Enabled {
		if update.TextDelta != "" {
			result.Trace.TextEvents++
		}
		if update.Finished {
			result.Trace.FinishEvents++
		}
		if update.TextDelta == "" && !update.Finished {
			fields := decodeMessage(payload)
			if len(payload) > 0 && len(fields) == 0 {
				result.Trace.MalformedFrames++
			} else if _, hasUpdate := fieldFirst(fields, 1); !hasUpdate {
				result.Trace.UnknownFrames++
			} else {
				result.Trace.OtherEvents++
			}
		}
	}
	if update.TextDelta != "" {
		result.Text += update.TextDelta
		if onDelta != nil {
			if err := onDelta(update.TextDelta); err != nil {
				result.Fatal = "cursor AgentService downstream write failed"
				if a.cancel != nil {
					a.cancel()
				}
				return
			}
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
