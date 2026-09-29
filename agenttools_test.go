package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestAgentValueNestedRoundTrip(t *testing.T) {
	input := map[string]any{
		"null":    nil,
		"array":   []any{nil, "引号 \" 和雪", float64(-12.375), true},
		"number":  float64(42.5),
		"boolean": false,
	}
	nodes := maxAgentValueNodes
	encoded, err := encodeAgentValue(input, 0, &nodes)
	if err != nil {
		t.Fatalf("encode Value: %v", err)
	}
	nodes = maxAgentValueNodes
	decoded, err := decodeAgentValue(encoded, 0, &nodes)
	if err != nil {
		t.Fatalf("decode Value: %v", err)
	}
	if !reflect.DeepEqual(decoded, input) {
		t.Fatalf("Value round trip mismatch\n got: %#v\nwant: %#v", decoded, input)
	}
}

func TestAgentToolCatalogEncodesSchemaAsGoogleValue(t *testing.T) {
	schema := `{"type":"object","properties":{"input":{"type":"array","items":{"type":"boolean"}},"count":{"type":"number"},"extra":{"type":"null"}},"required":["input"]}`
	catalog, err := newAgentToolCatalog([]cursorTool{{Name: "Bash", Description: "run a client tool", Arguments: schema}})
	if err != nil {
		t.Fatalf("newAgentToolCatalog: %v", err)
	}

	context, err := decodeAgentProto(catalog.contextFields())
	if err != nil {
		t.Fatalf("decode context fields: %v", err)
	}
	if len(context) != 1 || context[0].number != 7 || context[0].wire != wireLen {
		t.Fatalf("expected only RequestContext.tools field 7, got %#v", context)
	}
	definition, err := decodeAgentProto(context[0].bytes)
	if err != nil {
		t.Fatalf("decode definition: %v", err)
	}
	values, err := agentSingularFields(definition, nil)
	if err != nil {
		t.Fatalf("definition fields: %v", err)
	}
	if got := agentToolString(values, 1); got != "Bash" {
		t.Fatalf("registered name = %q, want Bash", got)
	}
	if got := agentToolString(values, 4); got != "cpa-client" {
		t.Fatalf("provider = %q, want cpa-client", got)
	}
	if got := agentToolString(values, 5); got != "Bash" {
		t.Fatalf("original tool name = %q, want Bash", got)
	}
	if got := agentToolString(values, 6); got != schema {
		t.Fatalf("optional JSON schema changed: %q", got)
	}

	nodes := maxAgentValueNodes
	decodedSchema, err := decodeAgentValue(values[3].bytes, 0, &nodes)
	if err != nil {
		t.Fatalf("decode input_schema Value: %v", err)
	}
	root, ok := decodedSchema.(map[string]any)
	if !ok || root["type"] != "object" {
		t.Fatalf("input_schema is not an object Value: %#v", decodedSchema)
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is not a nested Struct Value: %#v", root["properties"])
	}
	inputProperty, ok := properties["input"].(map[string]any)
	if !ok || inputProperty["type"] != "array" {
		t.Fatalf("array property was not preserved: %#v", properties["input"])
	}
	required, ok := root["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "input" {
		t.Fatalf("required array was not preserved: %#v", root["required"])
	}
}

func TestAgentMCPToolRequestAndResultCorrelation(t *testing.T) {
	catalog := testAgentCatalog(t)
	command := `printf "你好" && echo 'quoted'`
	requestBytes := testAgentMCPExec(t, 73, "exec-session-α", "Bash", "cpa-client", "Bash", "call-123", map[string]any{
		"command": command,
		"flag":    true,
		"count":   float64(3.5),
		"empty":   nil,
	})
	request, alreadyFramed, err := catalog.parseExec(requestBytes)
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if request == nil || alreadyFramed != nil {
		t.Fatalf("expected executable declared MCP request, got request=%#v response=%x", request, alreadyFramed)
	}
	if request.Name != "Bash" || request.toolCallID != "call-123" {
		t.Fatalf("request identity lost: %#v", request)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(request.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if got, ok := args["command"].(string); !ok || got != command {
		t.Fatalf("quoted Unicode argument changed: got %#v, want %q", args["command"], command)
	}
	if args["flag"] != true || args["count"] != 3.5 || args["empty"] != nil {
		t.Fatalf("MCP Value arguments were not preserved: %#v", args)
	}

	resultText := `完成 "成功" — 世界`
	response := testAgentUnwrapFrame(t, request.resultFrame(resultText, true))
	outer, err := decodeAgentProto(response)
	if err != nil {
		t.Fatalf("decode ExecClientMessage: %v", err)
	}
	outerValues, err := agentSingularFields(outer, nil)
	if err != nil {
		t.Fatalf("ExecClientMessage fields: %v", err)
	}
	if outerValues[1].value != 73 || agentToolString(outerValues, 15) != "exec-session-α" {
		t.Fatalf("ExecClientMessage correlation lost: %#v", outerValues)
	}
	mcpResult, ok := outerValues[11]
	if !ok {
		t.Fatalf("MCP result field 11 missing: %#v", outerValues)
	}
	mcp, err := decodeAgentProto(mcpResult.bytes)
	if err != nil {
		t.Fatalf("decode McpResult: %v", err)
	}
	mcpValues, err := agentSingularFields(mcp, nil)
	if err != nil {
		t.Fatalf("McpResult fields: %v", err)
	}
	success, ok := mcpValues[1]
	if !ok {
		t.Fatalf("McpResult.success field 1 missing: %#v", mcpValues)
	}
	successFields, err := decodeAgentProto(success.bytes)
	if err != nil {
		t.Fatalf("decode McpSuccess: %v", err)
	}
	var contentItem []byte
	isError := false
	for _, field := range successFields {
		switch field.number {
		case 1:
			contentItem = field.bytes
		case 2:
			isError = field.value == 1
		}
	}
	if !isError {
		t.Fatal("McpSuccess.is_error field 2 was not set")
	}
	itemFields, err := decodeAgentProto(contentItem)
	if err != nil || len(itemFields) != 1 || itemFields[0].number != 1 {
		t.Fatalf("McpSuccess.content item is malformed: %#v, err=%v", itemFields, err)
	}
	textFields, err := decodeAgentProto(itemFields[0].bytes)
	if err != nil || len(textFields) != 1 || textFields[0].number != 1 {
		t.Fatalf("McpTextContent is malformed: %#v, err=%v", textFields, err)
	}
	if got := string(textFields[0].bytes); got != resultText {
		t.Fatalf("tool result text changed: got %q, want %q", got, resultText)
	}
}

// Cursor 后端会把 provider_identifier 回填成 pi-agent 等值，它不参与授权判断。
func TestAgentMCPAcceptsDeclaredToolFromAnyProvider(t *testing.T) {
	catalog := testAgentCatalog(t)
	for _, test := range []struct{ provider, name, toolName string }{
		{provider: "pi-agent", name: "Bash", toolName: "Bash"},
		{provider: "", name: "Bash", toolName: ""},
		{provider: "other-provider", name: "mcp_other-provider_Bash", toolName: "Bash"},
	} {
		request, response, err := catalog.parseExec(testAgentMCPExec(t, 5, "exec-5", test.name, test.provider, test.toolName, "call-5", map[string]any{"command": "true"}))
		if err != nil || response != nil || request == nil || request.Name != "Bash" {
			t.Fatalf("%+v: expected executable Bash request, got request=%#v response=%x err=%v", test, request, response, err)
		}
	}
}

func TestAgentMCPRejectsUnlistedTool(t *testing.T) {
	catalog := testAgentCatalog(t)
	for _, test := range []struct {
		name     string
		provider string
		result   int
	}{
		{name: "unknown tool", provider: "cpa-client", result: 5},
		{name: "unknown tool from another provider", provider: "other-provider", result: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestBytes := testAgentMCPExec(t, 9, "exec-9", "NotDeclared", test.provider, "NotDeclared", "", nil)
			request, response, err := catalog.parseExec(requestBytes)
			if err != nil {
				t.Fatalf("parseExec: %v", err)
			}
			if request != nil || len(response) < 5 {
				t.Fatalf("expected safe framed rejection, got request=%#v response=%x", request, response)
			}
			outer := testAgentUnwrapFrame(t, response)
			outerFields, err := decodeAgentProto(outer)
			if err != nil {
				t.Fatalf("decode rejection response: %v", err)
			}
			values, err := agentSingularFields(outerFields, nil)
			if err != nil {
				t.Fatalf("response fields: %v", err)
			}
			mcpFields, err := decodeAgentProto(values[11].bytes)
			if err != nil || len(mcpFields) != 1 || mcpFields[0].number != test.result {
				t.Fatalf("expected McpResult field %d, got %#v, err=%v", test.result, mcpFields, err)
			}
		})
	}
}

func TestAgentMCPApprovalProbeIsRejected(t *testing.T) {
	catalog := testAgentCatalog(t)
	mcpArgs := concat(
		fieldString(1, "Bash"),
		fieldString(4, "cpa-client"),
		fieldString(5, "Bash"),
		fieldVarint(7, 1), // smart_mode_approval_only
	)
	request, response, err := catalog.parseExec(concat(fieldVarint(1, 8), fieldBytes(11, mcpArgs)))
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if request != nil {
		t.Fatalf("approval-only request became executable: %#v", request)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, response))
	if err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	values, err := agentSingularFields(outer, nil)
	if err != nil {
		t.Fatalf("response fields: %v", err)
	}
	result, err := decodeAgentProto(values[11].bytes)
	if err != nil || len(result) != 1 || result[0].number != 3 {
		t.Fatalf("approval probe did not return McpResult.rejected field 3: %#v, err=%v", result, err)
	}
}

func TestAgentMCPStateReportsDeclaredTools(t *testing.T) {
	catalog := testAgentCatalog(t)
	requestBytes := concat(
		fieldVarint(1, 44),
		fieldString(15, "state-exec"),
		fieldBytes(36, fieldVarint(2, 0)), // McpStateExecArgs.kick_only=false
	)
	request, response, err := catalog.parseExec(requestBytes)
	if err != nil {
		t.Fatalf("parseExec mcp_state_exec_args: %v", err)
	}
	if request != nil {
		t.Fatalf("mcp state query became executable: %#v", request)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, response))
	if err != nil {
		t.Fatalf("decode mcp state response: %v", err)
	}
	values, err := agentSingularFields(outer, nil)
	if err != nil {
		t.Fatalf("response fields: %v", err)
	}
	if values[1].value != 44 || agentToolString(values, 15) != "state-exec" {
		t.Fatalf("mcp state response lost correlation: %#v", values)
	}
	stateResult, err := decodeAgentProto(values[36].bytes)
	if err != nil || len(stateResult) != 1 || stateResult[0].number != 1 {
		t.Fatalf("expected McpStateExecResult.success field 1: %#v, err=%v", stateResult, err)
	}
	success, err := decodeAgentProto(stateResult[0].bytes)
	if err != nil || len(success) != 1 || success[0].number != 1 {
		t.Fatalf("expected McpStateSuccess.servers field 1: %#v, err=%v", success, err)
	}
	server, err := decodeAgentProto(success[0].bytes)
	if err != nil {
		t.Fatalf("decode McpStateServer: %v", err)
	}
	serverValues, err := agentSingularFields(server, map[int]bool{5: true})
	if err != nil {
		t.Fatalf("McpStateServer fields: %v", err)
	}
	if agentToolString(serverValues, 1) != "cpa-client" || agentToolString(serverValues, 2) != "cpa-client" {
		t.Fatalf("unexpected server identity: %#v", serverValues)
	}
	if agentToolString(serverValues, 7) != "connected" {
		t.Fatalf("unexpected MCP server status: %#v", serverValues[7])
	}
	toolCount := 0
	for _, field := range server {
		if field.number == 5 {
			toolCount++
		}
	}
	if toolCount != len(catalog.definitions) {
		t.Fatalf("mcp state tool count mismatch: got %d want %d", toolCount, len(catalog.definitions))
	}
}

func TestAgentMCPStateFiltersRequestedServers(t *testing.T) {
	catalog := testAgentCatalog(t)
	requestBytes := concat(fieldVarint(1, 45), fieldString(15, "state-request-45"), fieldBytes(36, fieldString(1, "other-server")))
	_, response, err := catalog.parseExec(requestBytes)
	if err != nil {
		t.Fatalf("parse mcp state query: %v", err)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, response))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	values, err := agentSingularFields(outer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if values[1].value != 45 || agentToolString(values, 15) != "state-request-45" {
		t.Fatalf("mcp state filter response lost correlation: %#v", values)
	}
	stateResult, err := decodeAgentProto(values[36].bytes)
	if err != nil || len(stateResult) != 1 || stateResult[0].number != 1 {
		t.Fatalf("expected McpStateExecResult.success: %#v, err=%v", stateResult, err)
	}
	success, err := decodeAgentProto(stateResult[0].bytes)
	if err != nil || len(success) != 0 {
		t.Fatalf("unknown requested server must yield no servers: %#v, err=%v", success, err)
	}
}

func TestAgentNativeToolsAreDeniedAndDeclaredMCPNamesAreMapped(t *testing.T) {
	catalog, err := newAgentToolCatalog([]cursorTool{{
		Name:      "Bash",
		Arguments: `{"type":"object","properties":{"command":{"type":"string"}}}`,
	}})
	if err != nil {
		t.Fatalf("newAgentToolCatalog: %v", err)
	}
	tests := []struct {
		name           string
		requestField   int
		args           []byte
		response       int
		resultField    int
		errorTextField int
	}{
		{name: "shell", requestField: 2, args: fieldString(1, "echo never-run"), response: 2, resultField: 7, errorTextField: 3},
		{name: "write", requestField: 3, args: concat(fieldString(1, "/tmp/never-write"), fieldString(2, "data")), response: 3, resultField: 3, errorTextField: 4},
		{name: "delete", requestField: 4, args: fieldString(1, "/tmp/never-delete"), response: 4, resultField: 4, errorTextField: 2},
		{name: "grep", requestField: 5, args: fieldString(1, "pattern"), response: 5, resultField: 2, errorTextField: 1},
		{name: "read", requestField: 7, args: fieldString(1, "/tmp/never-read"), response: 7, resultField: 2, errorTextField: 2},
		{name: "ls", requestField: 8, args: fieldString(1, "/tmp"), response: 8, resultField: 2, errorTextField: 2},
		{name: "diagnostics", requestField: 9, args: fieldString(1, "/tmp/never-read"), response: 9, resultField: 2, errorTextField: 2},
		{name: "shell stream", requestField: 14, args: fieldString(1, "echo never-run"), response: 14, resultField: 6, errorTextField: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := concat(fieldVarint(1, 31), fieldBytes(test.requestField, test.args))
			parsed, response, err := catalog.parseExec(request)
			if err != nil {
				t.Fatalf("parseExec: %v", err)
			}
			if parsed != nil {
				t.Fatalf("native tool became executable: %#v", parsed)
			}
			outerFields, err := decodeAgentProto(testAgentUnwrapFrame(t, response))
			if err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			outerValues, err := agentSingularFields(outerFields, nil)
			if err != nil {
				t.Fatalf("response fields: %v", err)
			}
			result, ok := outerValues[test.response]
			if !ok {
				t.Fatalf("native response field %d missing", test.response)
			}
			resultFields, err := decodeAgentProto(result.bytes)
			if err != nil || len(resultFields) != 1 || resultFields[0].number != test.resultField {
				t.Fatalf("expected safe result field %d, got %#v, err=%v", test.resultField, resultFields, err)
			}
			errorFields, err := decodeAgentProto(resultFields[0].bytes)
			if err != nil {
				t.Fatalf("decode native denial result: %v", err)
			}
			inner, err := agentSingularFields(errorFields, nil)
			if err != nil {
				t.Fatalf("native denial result fields: %v", err)
			}
			if got := agentToolString(inner, test.errorTextField); !strings.Contains(got, "MCP tool") {
				t.Fatalf("native result did not guide caller to MCP: field %d = %q", test.errorTextField, got)
			}
		})
	}

	// 注册名为 Bash 的 MCP 工具仍可被转成调用方待执行请求；仅原生 shell 被拒绝。
	parsed, response, err := catalog.parseExec(testAgentMCPExec(t, 32, "", "Bash", "cpa-client", "Bash", "", map[string]any{"command": "echo client"}))
	if err != nil || parsed == nil || response != nil || parsed.Name != "Bash" {
		t.Fatalf("declared Bash MCP tool was not mapped safely: request=%#v response=%x err=%v", parsed, response, err)
	}
}

func TestAgentNativeReadMapsToDeclaredReadAndReturnsTextOnly(t *testing.T) {
	catalog := testAgentCatalog(t)
	path := `/workspace/"quoted"/雪.go`
	requestBytes := concat(
		fieldVarint(1, 81),
		fieldString(15, "read-exec"),
		fieldBytes(7, concat(
			fieldString(1, path),
			fieldString(2, "native-read-call"),
			fieldVarint(4, 3),
			fieldVarint(5, 17),
		)),
	)
	request, response, err := catalog.parseExec(requestBytes)
	if err != nil {
		t.Fatalf("parse native Read: %v", err)
	}
	if request == nil || response != nil {
		t.Fatalf("expected a client-side Read request, got request=%#v response=%x", request, response)
	}
	if request.Name != "Read" || request.toolCallID != "native-read-call" || !request.nativeRead {
		t.Fatalf("native Read mapping lost identity: %#v", request)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(request.Arguments), &args); err != nil {
		t.Fatalf("mapped arguments are not JSON: %v", err)
	}
	if args["file_path"] != path || args["offset"] != float64(3) || args["limit"] != float64(17) {
		t.Fatalf("native Read arguments were not mapped exactly: %#v", args)
	}
	if _, exists := args["path"]; exists {
		t.Fatalf("unexpected extra path alias in mapped arguments: %#v", args)
	}

	content := `内容 "原样" 保留`
	outerFields, err := decodeAgentProto(testAgentUnwrapFrame(t, request.resultFrame(content, false)))
	if err != nil {
		t.Fatalf("decode native Read result: %v", err)
	}
	outerValues, err := agentSingularFields(outerFields, nil)
	if err != nil {
		t.Fatalf("response fields: %v", err)
	}
	if outerValues[1].value != 81 || agentToolString(outerValues, 15) != "read-exec" {
		t.Fatalf("native Read result correlation lost: %#v", outerValues)
	}
	readResult, ok := outerValues[7]
	if !ok {
		t.Fatalf("native Read result field 7 missing: %#v", outerValues)
	}
	resultFields, err := decodeAgentProto(readResult.bytes)
	if err != nil || len(resultFields) != 1 || resultFields[0].number != 1 {
		t.Fatalf("expected ReadResult.success field 1: %#v, err=%v", resultFields, err)
	}
	successFields, err := decodeAgentProto(resultFields[0].bytes)
	if err != nil {
		t.Fatalf("decode ReadSuccess: %v", err)
	}
	successValues, err := agentSingularFields(successFields, nil)
	if err != nil {
		t.Fatalf("ReadSuccess fields: %v", err)
	}
	if agentToolString(successValues, 1) != path || agentToolString(successValues, 2) != content {
		t.Fatalf("ReadSuccess path/content mismatch: %#v", successValues)
	}
	for _, metadataField := range []int{3, 4, 5, 6, 7} {
		if _, exists := successValues[metadataField]; exists {
			t.Fatalf("fabricated ReadSuccess metadata field %d", metadataField)
		}
	}

	failedFields, err := decodeAgentProto(testAgentUnwrapFrame(t, request.resultFrame("client read failed", true)))
	if err != nil {
		t.Fatalf("decode failed Read result: %v", err)
	}
	failedOuter, err := agentSingularFields(failedFields, nil)
	if err != nil {
		t.Fatalf("failed response fields: %v", err)
	}
	failedResult, err := decodeAgentProto(failedOuter[7].bytes)
	if err != nil || len(failedResult) != 1 || failedResult[0].number != 2 {
		t.Fatalf("expected ReadResult.error field 2: %#v, err=%v", failedResult, err)
	}
	failedError, err := decodeAgentProto(failedResult[0].bytes)
	if err != nil {
		t.Fatalf("decode ReadError: %v", err)
	}
	failedValues, err := agentSingularFields(failedError, nil)
	if err != nil || agentToolString(failedValues, 1) != path || agentToolString(failedValues, 2) != "client read failed" {
		t.Fatalf("ReadError did not preserve path/text: %#v, err=%v", failedValues, err)
	}
}

func TestAgentNativeReadMapsToReadFileAndRejectsIncompatibleSchema(t *testing.T) {
	readFile, err := newAgentToolCatalog([]cursorTool{{
		Name:      "read_file",
		Arguments: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
	}})
	if err != nil {
		t.Fatalf("newAgentToolCatalog: %v", err)
	}
	request, response, err := readFile.parseExec(concat(fieldVarint(1, 5), fieldBytes(7, fieldString(1, "/tmp/a"))))
	if err != nil || request == nil || response != nil || request.Name != "read_file" {
		t.Fatalf("compatible read_file was not mapped: request=%#v response=%x err=%v", request, response, err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(request.Arguments), &args); err != nil || args["path"] != "/tmp/a" {
		t.Fatalf("read_file path mapping failed: args=%#v err=%v", args, err)
	}

	incompatible, err := newAgentToolCatalog([]cursorTool{{
		Name:      "Read",
		Arguments: `{"type":"object","properties":{"file_path":{"type":"number"}},"required":["file_path"]}`,
	}})
	if err != nil {
		t.Fatalf("newAgentToolCatalog: %v", err)
	}
	parsed, framed, err := incompatible.parseExec(concat(fieldVarint(1, 6), fieldBytes(7, fieldString(1, "/tmp/a"))))
	if err != nil || parsed != nil || len(framed) == 0 {
		t.Fatalf("incompatible native Read was not safely rejected: request=%#v response=%x err=%v", parsed, framed, err)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, framed))
	if err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	values, err := agentSingularFields(outer, nil)
	if err != nil {
		t.Fatalf("response fields: %v", err)
	}
	readResult, err := decodeAgentProto(values[7].bytes)
	if err != nil || len(readResult) != 1 || readResult[0].number != 2 {
		t.Fatalf("expected ReadResult.error on incompatible schema: %#v, err=%v", readResult, err)
	}
	readError, err := decodeAgentProto(readResult[0].bytes)
	if err != nil || !strings.Contains(agentToolString(mustAgentSingular(t, readError), 2), "MCP") {
		t.Fatalf("native Read refusal did not guide to MCP: %#v, err=%v", readError, err)
	}
}

func mustAgentSingular(t *testing.T, fields []agentProtoField) map[int]agentProtoField {
	t.Helper()
	values, err := agentSingularFields(fields, nil)
	if err != nil {
		t.Fatalf("singular protobuf fields: %v", err)
	}
	return values
}

func TestAgentExecUnknownFieldDoesNotExposePayload(t *testing.T) {
	catalog := testAgentCatalog(t)
	request, reply, err := catalog.parseExec(concat(fieldVarint(1, 1), fieldBytes(99, []byte("sensitive-payload"))))
	if err != nil || request != nil || len(reply) == 0 {
		t.Fatalf("unknown exec kind should get a throw reply, got request=%#v reply=%x err=%v", request, reply, err)
	}
	if !bytes.Contains(reply, []byte("99")) || bytes.Contains(reply, []byte("sensitive-payload")) {
		t.Fatalf("throw should name only the exec type, got %q", reply)
	}
	first := int(binary.BigEndian.Uint32(reply[1:5]))
	outer, err := decodeAgentProto(reply[5 : 5+first])
	if err != nil || len(outer) != 1 || outer[0].number != 5 {
		t.Fatalf("expected AgentClientMessage.exec_client_control_message, got %#v err=%v", outer, err)
	}
}

// Cursor 新增 ShellArgs 字段（23）和 repeated 字段（22）时仍应回复权限拒绝，而不是让整轮失败。
func TestAgentNativeShellToleratesNewFields(t *testing.T) {
	catalog := testAgentCatalog(t)
	shell := concat(fieldString(1, "ls"), fieldString(22, "rm"), fieldString(22, "dd"), fieldBytes(23, fieldString(1, "new")))
	request, reply, err := catalog.parseExec(concat(fieldVarint(1, 3), fieldBytes(2, shell)))
	if err != nil || request != nil || len(reply) < 5 {
		t.Fatalf("expected permission-denied reply, got request=%#v reply=%x err=%v", request, reply, err)
	}
	outer, err := decodeAgentProto(testAgentUnwrapFrame(t, reply))
	if err != nil {
		t.Fatal(err)
	}
	values := mustAgentSingular(t, outer)
	if _, ok := values[2]; !ok {
		t.Fatalf("expected ShellResult, got %#v", outer)
	}
}

func TestAgentJSONSchemaAndValueLimits(t *testing.T) {
	for _, schema := range []string{`{"type":"object"} {"ignored":true}`, `[]`, `null`} {
		if _, err := newAgentToolCatalog([]cursorTool{{Name: "bad", Arguments: schema}}); err == nil {
			t.Fatalf("invalid schema accepted: %q", schema)
		}
	}
	for _, input := range [][]byte{
		fieldVarint(1, 1), // google.protobuf.NullValue must be zero
		concat(fieldVarint(4, 2)),
		concat(fieldFixed64(2, math.Float64bits(math.Inf(1)))),
	} {
		nodes := maxAgentValueNodes
		if _, err := decodeAgentValue(input, 0, &nodes); err == nil {
			t.Fatalf("invalid protobuf Value accepted: %x", input)
		}
	}
}

func testAgentCatalog(t *testing.T) *agentToolCatalog {
	t.Helper()
	catalog, err := newAgentToolCatalog([]cursorTool{
		{
			Name:        "Bash",
			Description: "client supplied command tool",
			Arguments:   `{"type":"object","properties":{"command":{"type":"string"}}}`,
		},
		{
			Name:      "Read",
			Arguments: `{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["file_path"]}`,
		},
		{
			Name:      "read_file",
			Arguments: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		},
	})
	if err != nil {
		t.Fatalf("newAgentToolCatalog: %v", err)
	}
	return catalog
}

func testAgentMCPExec(t *testing.T, id uint32, execID, name, provider, toolName, callID string, args map[string]any) []byte {
	t.Helper()
	var encodedArgs []byte
	for key, value := range args {
		nodes := maxAgentValueNodes
		encodedValue, err := encodeAgentValue(value, 0, &nodes)
		if err != nil {
			t.Fatalf("encode MCP test argument %q: %v", key, err)
		}
		entry := concat(fieldString(1, key), fieldBytes(2, encodedValue))
		encodedArgs = append(encodedArgs, fieldBytes(2, entry)...)
	}
	mcpArgs := concat(
		fieldString(1, name),
		encodedArgs,
		fieldString(3, callID),
		fieldString(4, provider),
		fieldString(5, toolName),
	)
	request := concat(fieldVarint(1, uint64(id)), fieldBytes(11, mcpArgs))
	if execID != "" {
		request = concat(fieldVarint(1, uint64(id)), fieldString(15, execID), fieldBytes(11, mcpArgs))
	}
	return request
}

func testAgentUnwrapFrame(t *testing.T, frame []byte) []byte {
	t.Helper()
	if len(frame) < 5 || frame[0] != 0 {
		t.Fatalf("not an uncompressed Connect frame: %x", frame)
	}
	length := binary.BigEndian.Uint32(frame[1:5])
	if int(length) != len(frame)-5 {
		t.Fatalf("Connect frame length mismatch: header=%d actual=%d", length, len(frame)-5)
	}
	outer, err := decodeAgentProto(frame[5:])
	if err != nil || len(outer) != 1 || outer[0].number != 2 || outer[0].wire != wireLen {
		t.Fatalf("expected AgentClientMessage.exec_client_message field 2, got %#v err=%v", outer, err)
	}
	return outer[0].bytes
}

func TestAgentInteractionResponse(t *testing.T) {
	for _, test := range []struct {
		kind    int
		handled bool
		want    []byte
	}{
		{kind: 2, handled: true, want: fieldBytes(1, nil)},
		{kind: 9, handled: true, want: fieldBytes(1, nil)},
		{kind: 4, handled: true, want: fieldBytes(2, fieldString(1, agentInteractionRejectReason))},
		{kind: 3, handled: true, want: fieldBytes(1, fieldBytes(3, fieldString(1, agentInteractionRejectReason)))},
		{kind: 7, handled: true, want: fieldBytes(1, fieldBytes(2, fieldString(1, agentInteractionRejectReason)))},
		{kind: 10, handled: true, want: fieldBytes(3, fieldString(1, agentInteractionRejectReason))},
		{kind: 8, handled: false},
	} {
		query := concat(fieldVarint(1, 42), fieldBytes(test.kind, fieldString(1, "q")))
		frame, handled := agentInteractionResponse(query)
		if handled != test.handled {
			t.Fatalf("kind %d: handled = %v, want %v", test.kind, handled, test.handled)
		}
		if !handled {
			continue
		}
		if len(frame) < 5 || frame[0] != 0 || int(binary.BigEndian.Uint32(frame[1:5])) != len(frame)-5 {
			t.Fatalf("kind %d: bad Connect frame %x", test.kind, frame)
		}
		outer, err := decodeAgentProto(frame[5:])
		if err != nil || len(outer) != 1 || outer[0].number != 6 {
			t.Fatalf("kind %d: expected AgentClientMessage.interaction_response, got %#v err=%v", test.kind, outer, err)
		}
		response, err := decodeAgentProto(outer[0].bytes)
		if err != nil || len(response) != 2 || response[0].number != 1 || response[0].value != 42 || response[1].number != test.kind {
			t.Fatalf("kind %d: bad InteractionResponse %#v err=%v", test.kind, response, err)
		}
		if !bytes.Equal(response[1].bytes, test.want) {
			t.Fatalf("kind %d: result = %x, want %x", test.kind, response[1].bytes, test.want)
		}
	}
}

// 未声明任何工具的纯文本请求里，模型发起原生 IDE 操作也只能收到拒绝，不能让整轮失败。
func TestAgentNativeExecWithoutDeclaredTools(t *testing.T) {
	pr, pw := io.Pipe()
	written := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		written <- buf[:n]
	}()
	client := &agentClient{pw: pw, context: &agentContext{blobs: map[string][]byte{}}}
	shell := fieldBytes(2, concat(fieldVarint(1, 7), fieldBytes(2, fieldString(1, "ls"))))
	var result agentRunResult
	client.handleAgentPayload(shell, &result, nil)
	if result.Fatal != "" {
		t.Fatalf("native exec without tools failed the turn: %s", result.Fatal)
	}
	if reply := <-written; len(reply) < 5 {
		t.Fatalf("expected a rejection frame, got %x", reply)
	}
	pw.Close()
}

// 上游 token_delta/turn_ended 必须进入 usage；只返回工具调用的轮次输出不能再是 0。
func TestAgentUsageFromUpstream(t *testing.T) {
	stats := &agentTurnStats{}
	client := &agentClient{}
	client.stats.Store(stats)
	client.observeUsage(fieldBytes(8, fieldVarint(1, 40)))
	client.observeUsage(fieldBytes(8, fieldVarint(1, 2)))
	client.observeUsage(fieldBytes(14, concat(fieldVarint(1, 1200), fieldVarint(2, 99), fieldVarint(3, 800))))
	var out []byte
	emitter := newChunkEmitter("m", false, false, func(b []byte) error { out = append([]byte(nil), b...); return nil })
	emitter.stats = stats
	if err := emitter.toolCall(&toolCallResult{ID: "call_x", Name: "Read", Arguments: `{"file_path":"a"}`, IsLast: true}); err != nil {
		t.Fatal(err)
	}
	var last []byte
	emitter.emit = func(b []byte) error {
		if string(b) != "[DONE]" {
			last = append([]byte(nil), b...)
		}
		return nil
	}
	if err := emitter.finishChunk(1 << 20); err != nil {
		t.Fatal(err)
	}
	_ = out
	var chunk struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Details    struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(last, &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Usage.Prompt != 1200 || chunk.Usage.Completion != 42 || chunk.Usage.Details.Cached != 800 {
		t.Fatalf("usage = %+v", chunk.Usage)
	}
}

// 思考流要实时转发为 reasoning_content；非流式聚合在 usage 含嵌套对象时不能丢掉结尾块。
func TestAgentThinkingForwardedAndAggregated(t *testing.T) {
	update, _, _ := decodeAgentServerMessage(fieldBytes(1, fieldBytes(4, fieldString(1, "pondering"))))
	if update.ThinkingDelta != "pondering" {
		t.Fatalf("thinking delta = %q", update.ThinkingDelta)
	}
	agg := newCompletionAggregator("m")
	emitter := newChunkEmitter("m", false, false, agg.consume)
	emitter.stats = &agentTurnStats{ended: true, inTokens: 50, cacheRead: 40, outTokens: 7}
	if err := emitter.reasoningDelta("pondering"); err != nil {
		t.Fatal(err)
	}
	if err := emitter.textDelta("answer"); err != nil {
		t.Fatal(err)
	}
	if err := emitter.finishChunk(1000); err != nil {
		t.Fatal(err)
	}
	raw, err := agg.completion()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	m := out.Choices[0].Message
	if m.Content != "answer" || m.Reasoning != "pondering" || out.Usage["prompt_tokens"] != float64(50) || out.Usage["prompt_tokens_details"] == nil {
		t.Fatalf("completion = %s", raw)
	}
}

// Cursor 报告的是 run 内各步累计输入；上报给客户端的上下文不能超过请求本身，否则客户端会
// 反复触发自动压缩。缓存比例保持上游真实值。
func TestAgentUsageCappedToRequestSize(t *testing.T) {
	var last []byte
	emitter := newChunkEmitter("m", false, false, func(b []byte) error {
		if string(b) != "[DONE]" {
			last = append([]byte(nil), b...)
		}
		return nil
	})
	emitter.stats = &agentTurnStats{ended: true, inTokens: 200000, cacheRead: 180000, outTokens: 5}
	if err := emitter.finishChunk(400000); err != nil {
		t.Fatal(err)
	}
	var chunk struct {
		Usage struct {
			Prompt  int `json:"prompt_tokens"`
			Details struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(last, &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Usage.Prompt != 100000 || chunk.Usage.Details.Cached != 90000 {
		t.Fatalf("usage = %+v", chunk.Usage)
	}
}
