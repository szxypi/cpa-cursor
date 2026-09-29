package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestAgentInvalidArgumentIsRequestFaultWithoutDiagnostics(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		body := []byte(`{"error":{"code":"invalid_argument","message":"unknown option '--system-prompt'","details":"` + string(bytes.Repeat([]byte("x"), 4096)) + `"}}`)
		if compressed {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			body = compressed.Bytes()
		}
		frame := wrapConnectFrame(body)
		frame[0] = 2
		if compressed {
			frame[0] = 3
		}
		result := agentRunResult{}
		(&agentClient{}).consumeAgentFrames(frame, &result, nil)
		err := classifyAgentFailure(result.Fatal)
		if !result.Ended || result.Fatal == "" || err.status != 400 || err.code != "invalid_request_error" {
			t.Fatalf("incorrect request error: %+v %+v", result, err)
		}
	}
}

func TestAgentDownstreamFailureCancelsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &agentClient{cancel: cancel}
	result := agentRunResult{}
	frame := wrapConnectFrame(fieldBytes(1, fieldBytes(1, fieldString(1, "synthetic text"))))
	client.consumeAgentFrames(frame, &result, func(string) error { return errors.New("closed") })
	if ctx.Err() == nil || result.Fatal == "" {
		t.Fatal("downstream failure did not cancel upstream")
	}
}

// 故意在 finish 和 trailer 之间分块，验证不能因 finish 提前返回成功。
type splitAgentReader struct{ first, second []byte }

func (r *splitAgentReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	if len(r.second) > 0 {
		n := copy(p, r.second)
		r.second = r.second[n:]
		return n, nil
	}
	return 0, io.EOF
}
func TestAgentReadsErrorTrailerAfterSeparateFinish(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	go io.Copy(io.Discard, r)
	finish := wrapConnectFrame(fieldBytes(1, fieldBytes(14, nil)))
	trailer := wrapConnectFrame([]byte(`{"error":{"code":"invalid_argument"}}`))
	trailer[0] = 2
	client := &agentClient{pw: w, resp: &http.Response{Body: io.NopCloser(&splitAgentReader{first: finish, second: trailer})}}
	result := client.runTurn([]byte("synthetic"), nil, false)
	if result.Fatal != "cursor AgentService invalid_argument" {
		t.Fatalf("lost trailer: %+v", result)
	}
}
func TestDecodeMessageTruncatedLength(t *testing.T) {
	for _, data := range [][]byte{{0x0a, 0x01}, {0x0a, 0x02, 'a'}, append([]byte{0x0a}, bytes.Repeat([]byte{0xff}, 10)...)} {
		if got := decodeMessage(data); len(got) != 0 {
			t.Fatalf("invalid frame accepted: %+v", got)
		}
	}
}
