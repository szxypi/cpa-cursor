package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func claudeCodeCatalog(t *testing.T) *agentToolCatalog {
	t.Helper()
	catalog, err := newAgentToolCatalog([]cursorTool{
		{Name: "Bash", Arguments: `{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"},"timeout":{"type":"number"},"run_in_background":{"type":"boolean"}},"required":["command"]}`},
		{Name: "Grep", Arguments: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"output_mode":{"type":"string"},"-B":{"type":"number"},"-A":{"type":"number"},"-C":{"type":"number"},"-i":{"type":"boolean"},"type":{"type":"string"},"head_limit":{"type":"number"},"multiline":{"type":"boolean"}},"required":["pattern"]}`},
		{Name: "Write", Arguments: `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func mappedArgs(t *testing.T, request *agentToolRequest) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(request.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	return args
}

// 原生 Shell/Grep/Write 要转成客户端声明的同类工具，并以原生结果类型告知上游已转交。
func TestAgentNativeExecMappedToClientTools(t *testing.T) {
	catalog := claudeCodeCatalog(t)

	shell := concat(fieldString(1, "go test ./..."), fieldString(2, "/tmp/it's"), fieldString(15, "run tests"), fieldString(22, "rm"))
	request, reply, err := catalog.parseExec(concat(fieldVarint(1, 3), fieldBytes(14, shell)))
	if err != nil || reply != nil || request == nil || request.Name != "Bash" {
		t.Fatalf("shell not mapped: request=%#v reply=%x err=%v", request, reply, err)
	}
	args := mappedArgs(t, request)
	if args["command"] != `cd '/tmp/it'\''s' && go test ./...` || args["description"] != "run tests" || len(args) != 2 {
		t.Fatalf("shell args = %v", args)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, request.handoffFrame()))
	if err != nil {
		t.Fatal(err)
	}
	if values := mustAgentSingular(t, outer); values[14].bytes == nil || !bytes.Contains(values[14].bytes, []byte("handed off to the client's Bash tool")) {
		t.Fatalf("shell handoff must be a ShellStream result naming the client tool: %#v", outer)
	}

	grep := concat(fieldString(1, "func main"), fieldString(2, "cmd"), fieldString(4, "files_with_matches"), fieldVarint(7, 2), fieldVarint(8, 1), fieldVarint(16, 5))
	request, _, err = catalog.parseExec(concat(fieldVarint(1, 4), fieldBytes(5, grep)))
	if err != nil || request == nil || request.Name != "Grep" {
		t.Fatalf("grep not mapped: %#v %v", request, err)
	}
	args = mappedArgs(t, request)
	if args["pattern"] != "func main" || args["path"] != "cmd" || args["output_mode"] != "files_with_matches" || args["-C"] != float64(2) || args["-i"] != true {
		t.Fatalf("grep args = %v", args)
	}
	if _, ok := args["offset"]; ok {
		t.Fatal("undeclared optional parameter must be dropped")
	}

	write := concat(fieldString(1, "/tmp/a.txt"), fieldString(2, "hello"))
	request, _, err = catalog.parseExec(concat(fieldVarint(1, 5), fieldBytes(3, write)))
	if err != nil || request == nil || request.Name != "Write" {
		t.Fatalf("write not mapped: %#v %v", request, err)
	}
	if args = mappedArgs(t, request); args["file_path"] != "/tmp/a.txt" || args["content"] != "hello" {
		t.Fatalf("write args = %v", args)
	}
}

// 没有兼容工具（或写入二进制内容）时保持拒绝，不能凭空执行。
func TestAgentNativeExecWithoutCompatibleToolStillRejected(t *testing.T) {
	catalog, err := newAgentToolCatalog([]cursorTool{{Name: "Bash", Arguments: `{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`}})
	if err != nil {
		t.Fatal(err)
	}
	request, reply, err := catalog.parseExec(concat(fieldVarint(1, 3), fieldBytes(2, fieldString(1, "ls"))))
	if err != nil || request != nil || len(reply) == 0 {
		t.Fatalf("incompatible Bash schema must be rejected: request=%#v err=%v", request, err)
	}
	write := concat(fieldString(1, "/tmp/a.bin"), fieldBytes(5, []byte{0, 1}))
	request, _, err = claudeCodeCatalog(t).parseExec(concat(fieldVarint(1, 5), fieldBytes(3, write)))
	if err != nil || request != nil {
		t.Fatalf("binary write must not be mapped: %#v %v", request, err)
	}
}

// 客户端只声明 Bash 时，原生 grep 以等价 rg 命令交给 Bash。
func TestAgentNativeGrepFallsBackToBashRipgrep(t *testing.T) {
	catalog, err := newAgentToolCatalog([]cursorTool{
		{Name: "Bash", Arguments: `{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"}},"required":["command"]}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	grep := concat(fieldString(1, "it's"), fieldString(2, "src"), fieldString(3, "*.go"), fieldString(4, "files_with_matches"), fieldVarint(8, 1), fieldVarint(10, 20))
	request, reply, err := catalog.parseExec(concat(fieldVarint(1, 6), fieldBytes(5, grep)))
	if err != nil || reply != nil || request == nil || request.Name != "Bash" {
		t.Fatalf("grep not mapped to Bash: %#v %x %v", request, reply, err)
	}
	if got := mappedArgs(t, request)["command"]; got != `rg -l -i --glob '*.go' -e 'it'\''s' -- 'src' | head -n 20` {
		t.Fatalf("command = %v", got)
	}
}
