package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

const (
	maxAgentToolNameBytes   = 1024
	maxAgentToolCallIDBytes = 4 << 10
	maxAgentToolCount       = 256
	maxAgentToolDescription = 64 << 10
	maxAgentToolSchemaBytes = 1 << 20
	maxAgentToolArgsBytes   = 1 << 20
	maxAgentToolResultBytes = 4 << 20
	maxAgentMessageBytes    = 8 << 20
	maxAgentCatalogBytes    = 4 << 20
	maxAgentValueDepth      = 64
	maxAgentValueNodes      = 100_000
	maxAgentProtoFields     = 100_000
)

const agentToolPermissionMessage = "Native Cursor tools are disabled. Use one of the client's declared MCP tools instead."

// agentToolCatalog 只公布调用方声明的工具，不把 Cursor 内置工具交给 CPA 执行。
type agentToolCatalog struct {
	tools       map[string]cursorTool
	definitions [][]byte
}

// agentToolRequest 是待交由原调用方执行的 MCP 工具调用；它保存 Agent RPC 的关联信息，
// 使下一次 HTTP 请求带回工具结果时可以回复原来的 Agent 执行请求。
type agentToolRequest struct {
	Name      string
	Arguments string

	id              uint32
	execID          string
	hasExecID       bool
	toolCallID      string
	provider        string
	toolName        string
	nativeRead      bool
	nativePath      string
	nativeOffsetSet bool
}

// newAgentToolCatalog 校验工具 schema，并按 Cursor 的 google.protobuf.Value 编码生成目录。
func newAgentToolCatalog(tools []cursorTool) (*agentToolCatalog, error) {
	if len(tools) > maxAgentToolCount {
		return nil, fmt.Errorf("too many agent tools (maximum %d)", maxAgentToolCount)
	}

	catalog := &agentToolCatalog{
		tools:       make(map[string]cursorTool, len(tools)),
		definitions: make([][]byte, 0, len(tools)),
	}
	var catalogBytes int
	for _, tool := range tools {
		if tool.Name == "" || len(tool.Name) > maxAgentToolNameBytes || !utf8.ValidString(tool.Name) {
			return nil, errors.New("agent tool name must be bounded non-empty UTF-8")
		}
		if len(tool.Description) > maxAgentToolDescription {
			return nil, fmt.Errorf("agent tool %q description exceeds the size limit", tool.Name)
		}
		if !utf8.ValidString(tool.Description) {
			return nil, fmt.Errorf("agent tool %q description is not UTF-8", tool.Name)
		}
		if _, exists := catalog.tools[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate agent tool name %q", tool.Name)
		}
		if len(tool.Arguments) == 0 || len(tool.Arguments) > maxAgentToolSchemaBytes || !utf8.ValidString(tool.Arguments) {
			return nil, fmt.Errorf("agent tool %q has an invalid schema size or encoding", tool.Name)
		}

		parsed, err := decodeAgentJSON(tool.Arguments)
		if err != nil {
			return nil, fmt.Errorf("agent tool %q has invalid schema JSON", tool.Name)
		}
		if _, ok := parsed.(map[string]any); !ok {
			return nil, fmt.Errorf("agent tool %q schema must be a JSON object", tool.Name)
		}
		nodes := maxAgentValueNodes
		inputSchema, err := encodeAgentValue(parsed, 0, &nodes)
		if err != nil {
			return nil, fmt.Errorf("agent tool %q schema cannot be encoded", tool.Name)
		}

		// name/tool_name 都保持调用方原名；provider 固定为本插件声明的身份。
		definition := concat(
			fieldString(1, tool.Name),
			fieldString(2, tool.Description),
			fieldBytes(3, inputSchema),
			fieldString(4, "cpa-client"),
			fieldString(5, tool.Name),
			fieldString(6, tool.Arguments),
		)
		if len(definition) > maxAgentToolSchemaBytes+maxAgentToolDescription+3*maxAgentToolNameBytes+128 {
			return nil, errors.New("agent tool definition exceeds the size limit")
		}
		catalogBytes += len(definition)
		if catalogBytes > maxAgentCatalogBytes {
			return nil, errors.New("agent tool catalog exceeds the size limit")
		}
		catalog.tools[tool.Name] = tool
		catalog.definitions = append(catalog.definitions, definition)
	}
	return catalog, nil
}

// contextFields 返回 RequestContext 中重复的 tools 字段 7，不附带任何推荐规则。
func (c *agentToolCatalog) contextFields() []byte {
	if c == nil || len(c.definitions) == 0 {
		return nil
	}
	var fields []byte
	for _, definition := range c.definitions {
		fields = append(fields, fieldBytes(7, definition)...)
	}
	return fields
}

// parseExec 解析裸 ExecServerMessage。只允许目录内的 MCP 工具成为可执行请求；
// Cursor 原生工具一律返回对应的安全拒绝结果，不会触达本地命令或文件系统。
func (c *agentToolCatalog) parseExec(exec []byte) (*agentToolRequest, []byte, error) {
	outer, err := decodeAgentProto(exec)
	if err != nil {
		return nil, nil, err
	}
	if len(exec) > maxAgentMessageBytes {
		return nil, nil, errors.New("ExecServerMessage exceeds the size limit")
	}

	var id uint32
	var execID string
	var hasExecID bool
	var hasID bool
	var execField *agentProtoField
	unknownExec := 0
	for i := range outer {
		field := outer[i]
		switch field.number {
		case 1:
			if hasID || field.wire != wireVarint || field.value > math.MaxUint32 {
				return nil, nil, errors.New("invalid ExecServerMessage field 1")
			}
			hasID = true
			id = uint32(field.value)
		case 15:
			if hasExecID || field.wire != wireLen || !utf8.Valid(field.bytes) {
				return nil, nil, errors.New("invalid ExecServerMessage field 15")
			}
			hasExecID = true
			execID = string(field.bytes)
		case 19:
			if field.wire != wireLen {
				return nil, nil, errors.New("invalid ExecServerMessage field 19")
			}
		case 55:
			if field.wire != wireVarint || field.value > 1 {
				return nil, nil, errors.New("invalid ExecServerMessage field 55")
			}
		case 2, 3, 4, 5, 7, 8, 9, 11, 14, 29, 36:
			if field.wire != wireLen {
				return nil, nil, fmt.Errorf("invalid ExecServerMessage field %d", field.number)
			}
			if execField != nil {
				return nil, nil, fmt.Errorf("multiple ExecServerMessage tool fields %d and %d", execField.number, field.number)
			}
			execField = &field
		default:
			// 未知的 oneof 成员（Cursor 新增或未支持的执行类型）按 throw 处理；
			// 其余未知的非 oneof 字段忽略。
			if field.wire == wireLen && unknownExec == 0 {
				unknownExec = field.number
			}
		}
	}
	if !hasID {
		return nil, nil, errors.New("ExecServerMessage is missing field 1")
	}
	if execField == nil {
		if unknownExec != 0 {
			return nil, agentExecThrowFrames(id, fmt.Sprintf("Cursor exec type %d is not supported by this client; use one of the client's declared tools.", unknownExec)), nil
		}
		return nil, nil, errors.New("ExecServerMessage has no supported tool field")
	}

	if execField.number == 11 {
		return c.parseMCPExec(id, execID, hasExecID, execField.bytes)
	}
	if execField.number == 36 {
		response, err := c.mcpStateFrame(id, execID, hasExecID, execField.bytes)
		if err != nil {
			return nil, nil, err
		}
		return nil, response, nil
	}
	nativeArgs, err := decodeAgentProto(execField.bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid %s: %w", nativeArgsName(execField.number), err)
	}
	if err := validateNativeArgs(nativeArgs, execField.number); err != nil {
		return nil, nil, err
	}
	if execField.number == 7 {
		if request, ok, err := c.mapNativeRead(id, execID, hasExecID, nativeArgs); err != nil {
			return nil, nil, err
		} else if ok {
			return request, nil, nil
		}
	}
	if frame := nativeToolRejectionFrame(id, execID, hasExecID, *execField); frame != nil {
		return nil, frame, nil
	}
	return nil, agentExecThrowFrames(id, agentToolPermissionMessage), nil
}

// mcpStateFrame 回复 mcp_state_exec_args=36 的状态查询，只返回本目录声明的工具。
// kick_only 仅按 bool 校验；不根据未确认的语义伪造服务器状态。
func (c *agentToolCatalog) mcpStateFrame(id uint32, execID string, hasExecID bool, payload []byte) ([]byte, error) {
	fields, err := decodeAgentProto(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid McpStateExecArgs: %w", err)
	}
	if err := validateAgentWireFields(fields, "McpStateExecArgs", map[int]int{1: wireLen, 2: wireVarint}); err != nil {
		return nil, err
	}
	if _, err := agentSingularFields(fields, map[int]bool{1: true}); err != nil {
		return nil, fmt.Errorf("invalid McpStateExecArgs: %w", err)
	}
	serverIdentifiers := make(map[string]bool)
	for _, field := range fields {
		if field.number == 1 {
			if !utf8.Valid(field.bytes) || len(field.bytes) > maxAgentToolNameBytes {
				return nil, errors.New("invalid McpStateExecArgs server identifier")
			}
			serverIdentifiers[string(field.bytes)] = true
		}
	}
	for _, field := range fields {
		if field.number == 2 && field.value > 1 {
			return nil, errors.New("invalid McpStateExecArgs field 2 boolean")
		}
	}

	var success []byte
	if len(serverIdentifiers) == 0 || serverIdentifiers["cpa-client"] {
		var server []byte
		server = append(server, fieldString(1, "cpa-client")...)
		server = append(server, fieldString(2, "cpa-client")...)
		for _, definition := range c.definitions {
			server = append(server, fieldBytes(5, definition)...)
		}
		server = append(server, fieldString(7, "connected")...)
		if len(server) > maxAgentCatalogBytes {
			return nil, errors.New("McpStateServer exceeds the size limit")
		}
		success = fieldBytes(1, server)
	}
	result := fieldBytes(1, success) // McpStateExecResult.success
	response := encodeAgentClientMessage(id, execID, hasExecID, 36, result)
	if len(response) > maxAgentMessageBytes {
		return nil, errors.New("McpStateExecResult exceeds the size limit")
	}
	return wrapConnectFrame(response), nil
}

func (c *agentToolCatalog) parseMCPExec(id uint32, execID string, hasExecID bool, payload []byte) (*agentToolRequest, []byte, error) {
	fields, err := decodeAgentProto(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid McpArgs: %w", err)
	}
	spec := map[int]int{
		1: wireLen, 2: wireLen, 3: wireLen, 4: wireLen, 5: wireLen,
		6: wireLen, 7: wireVarint, 8: wireVarint, 9: wireLen,
	}
	if err := validateAgentWireFields(fields, "McpArgs", spec); err != nil {
		return nil, nil, err
	}
	values, err := agentSingularFields(fields, map[int]bool{2: true})
	if err != nil {
		return nil, nil, fmt.Errorf("invalid McpArgs: %w", err)
	}
	for _, number := range []int{7, 8} {
		if value, ok := values[number]; ok && value.value > 1 {
			return nil, nil, fmt.Errorf("invalid McpArgs field %d boolean", number)
		}
	}
	for _, number := range []int{1, 3, 4, 5, 9} {
		if value, ok := values[number]; ok && !utf8.Valid(value.bytes) {
			return nil, nil, fmt.Errorf("McpArgs field %d is not UTF-8", number)
		}
	}
	for _, limit := range []struct{ number, max int }{
		{1, maxAgentToolNameBytes},
		{3, maxAgentToolCallIDBytes},
		{4, maxAgentToolNameBytes},
		{5, maxAgentToolNameBytes},
		{9, maxAgentToolNameBytes},
	} {
		if value, ok := values[limit.number]; ok && len(value.bytes) > limit.max {
			return nil, nil, fmt.Errorf("McpArgs field %d exceeds the size limit", limit.number)
		}
	}
	name := agentToolString(values, 1)
	provider := agentToolString(values, 4)
	toolName := agentToolString(values, 5)
	callID := agentToolString(values, 3)

	// 权限探测始终拒绝，不能因客户端请求探测而伪造已批准状态。
	if approvalOnly, ok := values[7]; ok && approvalOnly.value != 0 {
		return nil, agentMCPRejectedFrame(id, execID, hasExecID, "Approval-only MCP requests are not executable."), nil
	}
	// provider_identifier 由 Cursor 后端回填（实测会给出 pi-agent 等非本插件声明的值），
	// 不是身份凭证；可执行性只取决于工具名是否在本次调用方声明的目录内。
	if c == nil {
		return nil, agentMCPToolNotFoundFrame(id, execID, hasExecID, name, nil), nil
	}
	resolved := ""
	for _, candidate := range []string{toolName, name} {
		if _, exists := c.tools[candidate]; candidate != "" && exists {
			resolved = candidate
			break
		}
	}
	if resolved == "" {
		return nil, agentMCPToolNotFoundFrame(id, execID, hasExecID, name, c), nil
	}
	name = resolved

	arguments, err := decodeMCPArgumentMap(fields)
	if err != nil {
		return nil, nil, err
	}
	argumentJSON, err := json.Marshal(arguments)
	if err != nil {
		return nil, nil, errors.New("McpArgs arguments cannot be represented as JSON")
	}
	if len(argumentJSON) > maxAgentToolArgsBytes {
		return nil, nil, errors.New("McpArgs arguments exceed the size limit")
	}
	return &agentToolRequest{
		Name:       name,
		Arguments:  string(argumentJSON),
		id:         id,
		execID:     execID,
		hasExecID:  hasExecID,
		toolCallID: callID,
		provider:   provider,
		toolName:   name,
	}, nil, nil
}

// mapNativeRead 仅在 Read/read_file 声明明确接受对应参数时将 IDE Read 转发给调用方。
// 它本身不读取文件；无法证明 schema 对应关系时返回 ok=false，由上层安全拒绝。
func (c *agentToolCatalog) mapNativeRead(id uint32, execID string, hasExecID bool, fields []agentProtoField) (*agentToolRequest, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	values, err := agentSingularFields(fields, nil)
	if err != nil {
		return nil, false, fmt.Errorf("invalid ReadArgs: %w", err)
	}
	pathField, hasPath := values[1]
	if !hasPath || !utf8.Valid(pathField.bytes) || len(pathField.bytes) == 0 {
		return nil, false, nil
	}
	path := string(pathField.bytes)
	callID := agentToolString(values, 2)
	if !utf8.ValidString(callID) {
		return nil, false, errors.New("ReadArgs field 2 is not UTF-8")
	}

	for _, name := range []string{"Read", "read_file"} {
		tool, exists := c.tools[name]
		if !exists {
			continue
		}
		schema, err := decodeAgentJSON(tool.Arguments)
		if err != nil {
			continue
		}
		root, ok := schema.(map[string]any)
		if !ok {
			continue
		}
		properties, ok := root["properties"].(map[string]any)
		if !ok {
			continue
		}
		pathName := ""
		for _, candidate := range []string{"file_path", "path"} {
			if property, ok := properties[candidate].(map[string]any); ok && property["type"] == "string" {
				if pathName != "" {
					pathName = ""
					break
				}
				pathName = candidate
			}
		}
		if pathName == "" {
			continue
		}
		arguments := map[string]any{pathName: path}
		compatible := true
		for _, parameter := range []struct {
			field int
			name  string
		}{
			{field: 4, name: "offset"},
			{field: 5, name: "limit"},
		} {
			value, present := values[parameter.field]
			if !present {
				continue
			}
			if parameter.name == "offset" && value.value > math.MaxInt32 || parameter.name == "limit" && value.value > math.MaxUint32 {
				compatible = false
				break
			}
			property, ok := properties[parameter.name].(map[string]any)
			if !ok || property["type"] != "integer" {
				compatible = false
				break
			}
			arguments[parameter.name] = json.Number(strconv.FormatUint(value.value, 10))
		}
		if !compatible {
			continue
		}
		if value, present := values[6]; present {
			property, ok := properties["encoding_hint"].(map[string]any)
			if !ok || property["type"] != "string" || !utf8.Valid(value.bytes) {
				continue
			}
			arguments["encoding_hint"] = string(value.bytes)
		}
		if !agentSchemaRequires(root, pathName) || !agentSchemaRequiredPropertiesMapped(root, arguments) || !agentSchemaOnlyAllowsMappedProperties(root, arguments) || !agentSchemaTypesAccept(root, arguments) {
			continue
		}
		argumentJSON, err := json.Marshal(arguments)
		if err != nil || len(argumentJSON) > maxAgentToolArgsBytes {
			continue
		}
		return &agentToolRequest{
			Name:       name,
			Arguments:  string(argumentJSON),
			id:         id,
			execID:     execID,
			hasExecID:  hasExecID,
			toolCallID: callID,
			provider:   "cpa-client",
			toolName:   name,
			nativeRead: true,
			nativePath: path,
		}, true, nil
	}
	return nil, false, nil
}

func agentSchemaRequires(schema map[string]any, name string) bool {
	required, ok := schema["required"].([]any)
	if !ok {
		return false
	}
	for _, item := range required {
		if item == name {
			return true
		}
	}
	return false
}

func agentSchemaOnlyAllowsMappedProperties(schema map[string]any, arguments map[string]any) bool {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return false
	}
	for name := range arguments {
		if _, declared := properties[name]; !declared {
			return false
		}
	}
	return true
}

func agentSchemaTypesAccept(schema map[string]any, arguments map[string]any) bool {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return false
	}
	for name, value := range arguments {
		property, ok := properties[name].(map[string]any)
		if !ok {
			return false
		}
		typeName, _ := property["type"].(string)
		switch typeName {
		case "string":
			if _, ok := value.(string); !ok {
				return false
			}
		case "integer":
			number, ok := value.(json.Number)
			if !ok {
				return false
			}
			parsed, err := strconv.ParseInt(string(number), 10, 64)
			if err != nil {
				return false
			}
			_ = parsed
		case "number":
			if _, ok := value.(json.Number); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func agentSchemaRequiredPropertiesMapped(schema map[string]any, arguments map[string]any) bool {
	required, ok := schema["required"].([]any)
	if !ok {
		return false
	}
	for _, item := range required {
		name, ok := item.(string)
		if !ok {
			return false
		}
		if _, mapped := arguments[name]; !mapped {
			return false
		}
	}
	return true
}

// resultFrame 把原调用方返回的文本封装为对应 Cursor 工具结果。
func (r *agentToolRequest) resultFrame(text string, isError bool) []byte {
	if r == nil {
		return nil
	}
	tooLarge := len(text) > maxAgentToolResultBytes
	if tooLarge || !utf8.ValidString(text) {
		text = "Tool result was not forwarded because it exceeded the supported size or encoding limits."
		isError = true
	}
	if r.nativeRead {
		var result []byte
		if isError {
			result = fieldBytes(2, concat(fieldString(1, r.nativePath), fieldString(2, text)))
		} else {
			// total_lines/file_size 等值由客户端提供，但纯文本结果不包含这些元数据，故不伪造。
			success := concat(fieldString(1, r.nativePath), fieldString(2, text))
			result = fieldBytes(1, success)
		}
		return wrapConnectFrame(encodeAgentClientMessage(r.id, r.execID, r.hasExecID, 7, result))
	}
	// McpSuccess.content 是 repeated McpToolResultContentItem；其 text oneof
	// 再包一层 McpTextContent{ text=1 }，不是直接 string。
	textContent := fieldString(1, text)
	contentItem := fieldBytes(1, textContent)
	success := fieldBytes(1, contentItem)
	if isError {
		success = append(success, fieldVarint(2, 1)...)
	}
	mcpResult := fieldBytes(1, success)
	return wrapConnectFrame(encodeAgentClientMessage(r.id, r.execID, r.hasExecID, 11, mcpResult))
}

func decodeMCPArgumentMap(fields []agentProtoField) (map[string]any, error) {
	args := make(map[string]any)
	totalBytes := 0
	nodes := maxAgentValueNodes
	for _, field := range fields {
		if field.number != 2 {
			continue
		}
		totalBytes += len(field.bytes)
		if totalBytes > maxAgentToolArgsBytes {
			return nil, errors.New("McpArgs argument map exceeds the size limit")
		}
		entry, err := decodeAgentProto(field.bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid McpArgs map entry: %w", err)
		}
		if err := validateAgentWireFields(entry, "McpArgs map entry", map[int]int{1: wireLen, 2: wireLen}); err != nil {
			return nil, err
		}
		entryValues, err := agentSingularFields(entry, nil)
		if err != nil {
			return nil, fmt.Errorf("invalid McpArgs map entry: %w", err)
		}
		keyField, hasKey := entryValues[1]
		valueField, hasValue := entryValues[2]
		key := ""
		if hasKey {
			if !utf8.Valid(keyField.bytes) {
				return nil, errors.New("McpArgs map key is not UTF-8")
			}
			key = string(keyField.bytes)
		}
		if _, exists := args[key]; exists {
			return nil, errors.New("McpArgs contains a duplicate argument name")
		}
		var rawValue []byte
		if hasValue {
			rawValue = valueField.bytes
		}
		decoded, err := decodeAgentValue(rawValue, 0, &nodes)
		if err != nil {
			return nil, fmt.Errorf("invalid McpArgs value for field 2: %w", err)
		}
		args[key] = decoded
	}
	return args, nil
}

func decodeAgentJSON(document string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(document)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

// encodeAgentValue 实现 google.protobuf.Value 的 oneof/Struct/ListValue wire 编码。
func encodeAgentValue(value any, depth int, nodes *int) ([]byte, error) {
	if depth > maxAgentValueDepth || *nodes <= 0 {
		return nil, errors.New("Value nesting limit exceeded")
	}
	*nodes--
	switch value := value.(type) {
	case nil:
		return fieldVarint(1, 0), nil
	case bool:
		if value {
			return fieldVarint(4, 1), nil
		}
		return fieldVarint(4, 0), nil
	case string:
		if !utf8.ValidString(value) {
			return nil, errors.New("invalid UTF-8 string")
		}
		return fieldString(3, value), nil
	case json.Number:
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, errors.New("invalid number")
		}
		return fieldFixed64(2, math.Float64bits(number)), nil
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return nil, errors.New("invalid number")
		}
		return fieldFixed64(2, math.Float64bits(value)), nil
	case []any:
		var list []byte
		for _, item := range value {
			encoded, err := encodeAgentValue(item, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			list = append(list, fieldBytes(1, encoded)...)
		}
		return fieldBytes(6, list), nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			if !utf8.ValidString(key) {
				return nil, errors.New("invalid UTF-8 object key")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var fields []byte
		for _, key := range keys {
			encoded, err := encodeAgentValue(value[key], depth+1, nodes)
			if err != nil {
				return nil, err
			}
			entry := concat(fieldString(1, key), fieldBytes(2, encoded))
			fields = append(fields, fieldBytes(1, entry)...)
		}
		return fieldBytes(5, fields), nil
	default:
		return nil, fmt.Errorf("unsupported JSON Value type %T", value)
	}
}

func decodeAgentValue(data []byte, depth int, nodes *int) (any, error) {
	if depth > maxAgentValueDepth || *nodes <= 0 {
		return nil, errors.New("Value nesting limit exceeded")
	}
	*nodes--
	fields, err := decodeAgentProto(data)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, nil // google.protobuf.Value 默认值为 null。
	}
	if len(fields) != 1 {
		return nil, errors.New("Value has multiple oneof fields")
	}
	field := fields[0]
	switch field.number {
	case 1:
		if field.wire != wireVarint || field.value != 0 {
			return nil, errors.New("invalid Value null field 1")
		}
		return nil, nil
	case 2:
		if field.wire != wireFixed64 {
			return nil, errors.New("invalid Value number field 2")
		}
		number := math.Float64frombits(field.value)
		if math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, errors.New("Value number field 2 is not finite")
		}
		return number, nil
	case 3:
		if field.wire != wireLen || !utf8.Valid(field.bytes) {
			return nil, errors.New("invalid Value string field 3")
		}
		return string(field.bytes), nil
	case 4:
		if field.wire != wireVarint || field.value > 1 {
			return nil, errors.New("invalid Value boolean field 4")
		}
		return field.value == 1, nil
	case 5:
		if field.wire != wireLen {
			return nil, errors.New("invalid Value struct field 5")
		}
		return decodeAgentStruct(field.bytes, depth, nodes)
	case 6:
		if field.wire != wireLen {
			return nil, errors.New("invalid Value list field 6")
		}
		return decodeAgentList(field.bytes, depth, nodes)
	default:
		return nil, fmt.Errorf("unknown Value field %d", field.number)
	}
}

func decodeAgentStruct(data []byte, depth int, nodes *int) (map[string]any, error) {
	fields, err := decodeAgentProto(data)
	if err != nil {
		return nil, err
	}
	if err := validateAgentWireFields(fields, "Struct", map[int]int{1: wireLen}); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(fields))
	for _, field := range fields {
		entry, err := decodeAgentProto(field.bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid Struct field 1: %w", err)
		}
		if err := validateAgentWireFields(entry, "Struct map entry", map[int]int{1: wireLen, 2: wireLen}); err != nil {
			return nil, err
		}
		values, err := agentSingularFields(entry, nil)
		if err != nil {
			return nil, fmt.Errorf("invalid Struct map entry: %w", err)
		}
		key := ""
		if keyField, ok := values[1]; ok {
			if !utf8.Valid(keyField.bytes) {
				return nil, errors.New("Struct key is not UTF-8")
			}
			key = string(keyField.bytes)
		}
		if _, exists := result[key]; exists {
			return nil, errors.New("Struct contains a duplicate key")
		}
		var raw []byte
		if valueField, ok := values[2]; ok {
			raw = valueField.bytes
		}
		value, err := decodeAgentValue(raw, depth+1, nodes)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func decodeAgentList(data []byte, depth int, nodes *int) ([]any, error) {
	fields, err := decodeAgentProto(data)
	if err != nil {
		return nil, err
	}
	if err := validateAgentWireFields(fields, "ListValue", map[int]int{1: wireLen}); err != nil {
		return nil, err
	}
	result := make([]any, 0, len(fields))
	for _, field := range fields {
		value, err := decodeAgentValue(field.bytes, depth+1, nodes)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

// strict wire reader 补足现有 pbField 不保存 wire type 的限制，避免把 fixed64 等字段误认成 varint。
type agentProtoField struct {
	number int
	wire   int
	bytes  []byte
	value  uint64
}

func decodeAgentProto(data []byte) ([]agentProtoField, error) {
	if len(data) > maxAgentMessageBytes {
		return nil, errors.New("protobuf message exceeds the size limit")
	}
	fields := make([]agentProtoField, 0)
	for offset := 0; offset < len(data); {
		tag, next, ok := readAgentVarint(data, offset)
		if !ok {
			return nil, errors.New("malformed protobuf tag")
		}
		offset = next
		number := int(tag >> 3)
		wire := int(tag & 7)
		if number == 0 || number > (1<<29)-1 {
			return nil, errors.New("invalid protobuf field number")
		}
		field := agentProtoField{number: number, wire: wire}
		switch wire {
		case wireVarint:
			field.value, offset, ok = readAgentVarint(data, offset)
			if !ok {
				return nil, fmt.Errorf("malformed protobuf field %d", number)
			}
		case wireFixed64:
			if len(data)-offset < 8 {
				return nil, fmt.Errorf("truncated protobuf field %d", number)
			}
			field.value = binary.LittleEndian.Uint64(data[offset : offset+8])
			offset += 8
		case wireLen:
			length, afterLength, lengthOK := readAgentVarint(data, offset)
			if !lengthOK || length > uint64(len(data)-afterLength) {
				return nil, fmt.Errorf("invalid protobuf length for field %d", number)
			}
			offset = afterLength
			field.bytes = data[offset : offset+int(length)]
			offset += int(length)
		case wireFixed32:
			if len(data)-offset < 4 {
				return nil, fmt.Errorf("truncated protobuf field %d", number)
			}
			field.value = uint64(binary.LittleEndian.Uint32(data[offset : offset+4]))
			offset += 4
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d for field %d", wire, number)
		}
		fields = append(fields, field)
		if len(fields) > maxAgentProtoFields {
			return nil, errors.New("protobuf message has too many fields")
		}
	}
	return fields, nil
}

func readAgentVarint(data []byte, offset int) (uint64, int, bool) {
	var value uint64
	for index := 0; index < 10 && offset < len(data); index++ {
		b := data[offset]
		offset++
		if index == 9 && b > 1 {
			return 0, offset, false
		}
		value |= uint64(b&0x7f) << (7 * index)
		if b&0x80 == 0 {
			return value, offset, true
		}
	}
	return 0, offset, false
}

func fieldFixed64(field int, value uint64) []byte {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	return concat(encodeVarint(uint64(field)<<3|wireFixed64), encoded[:])
}

func validateAgentWireFields(fields []agentProtoField, message string, spec map[int]int) error {
	for _, field := range fields {
		want, exists := spec[field.number]
		if !exists {
			return fmt.Errorf("unknown %s field %d", message, field.number)
		}
		if field.wire != want {
			return fmt.Errorf("invalid %s field %d wire type", message, field.number)
		}
	}
	return nil
}

// agentSingularFields 返回单值字段；repeated 字段需由调用方单独读取。
func agentSingularFields(fields []agentProtoField, repeated map[int]bool) (map[int]agentProtoField, error) {
	values := make(map[int]agentProtoField, len(fields))
	for _, field := range fields {
		if repeated[field.number] {
			continue
		}
		if _, exists := values[field.number]; exists {
			return nil, fmt.Errorf("duplicate protobuf field %d", field.number)
		}
		values[field.number] = field
	}
	return values, nil
}

func agentToolString(fields map[int]agentProtoField, number int) string {
	if field, ok := fields[number]; ok {
		return string(field.bytes)
	}
	return ""
}

func nativeToolRejectionFrame(id uint32, execID string, hasExecID bool, request agentProtoField) []byte {
	var resultField int
	var result []byte
	path, command, workingDirectory := "", "", ""
	args, err := decodeAgentProto(request.bytes)
	if err != nil {
		return nil
	}
	_, pathField := nativeArgsSpec(request.number)
	if err := validateNativeArgs(args, request.number); err != nil {
		return nil
	}
	fields := make(map[int]agentProtoField, len(args))
	for _, field := range args {
		if _, seen := fields[field.number]; !seen {
			fields[field.number] = field
		}
	}
	if pathField != 0 {
		if value, ok := fields[pathField]; ok && utf8.Valid(value.bytes) {
			path = string(value.bytes)
		}
	}
	if request.number == 2 || request.number == 14 {
		if value, ok := fields[1]; ok && utf8.Valid(value.bytes) {
			command = string(value.bytes)
		}
		if value, ok := fields[2]; ok && utf8.Valid(value.bytes) {
			workingDirectory = string(value.bytes)
		}
	}

	switch request.number {
	case 2: // ShellResult.permission_denied
		resultField = 2
		result = fieldBytes(7, concat(fieldString(1, command), fieldString(2, workingDirectory), fieldString(3, agentToolPermissionMessage)))
	case 14: // ShellStream.permission_denied
		resultField = 14
		result = fieldBytes(6, concat(fieldString(1, command), fieldString(2, workingDirectory), fieldString(3, agentToolPermissionMessage)))
	case 3: // WriteResult.permission_denied
		resultField = 3
		result = fieldBytes(3, concat(fieldString(1, path), fieldString(4, agentToolPermissionMessage)))
	case 4: // DeleteResult.permission_denied
		resultField = 4
		result = fieldBytes(4, concat(fieldString(1, path), fieldString(2, agentToolPermissionMessage)))
	case 5: // GrepResult.error
		resultField = 5
		result = fieldBytes(2, fieldString(1, agentToolPermissionMessage))
	case 7, 29: // ReadResult.error, including redacted reads
		resultField = request.number
		result = fieldBytes(2, concat(fieldString(1, path), fieldString(2, agentToolPermissionMessage)))
	case 8: // LsResult.error
		resultField = 8
		result = fieldBytes(2, concat(fieldString(1, path), fieldString(2, agentToolPermissionMessage)))
	case 9: // DiagnosticsResult.error
		resultField = 9
		result = fieldBytes(2, concat(fieldString(1, path), fieldString(2, agentToolPermissionMessage)))
	default:
		return nil
	}
	return wrapConnectFrame(encodeAgentClientMessage(id, execID, hasExecID, resultField, result))
}

func nativeArgsName(field int) string {
	switch field {
	case 2, 14:
		return "ShellArgs"
	case 3:
		return "WriteArgs"
	case 4:
		return "DeleteArgs"
	case 5:
		return "GrepArgs"
	case 7, 29:
		return "ReadArgs"
	case 8:
		return "LsArgs"
	case 9:
		return "DiagnosticsArgs"
	default:
		return "native tool args"
	}
}

func nativeArgsSpec(field int) (map[int]int, int) {
	switch field {
	case 2, 14:
		return map[int]int{1: wireLen, 2: wireLen, 3: wireVarint, 4: wireLen, 5: wireLen, 6: wireVarint, 7: wireVarint, 8: wireLen, 9: wireLen, 10: wireVarint, 11: wireVarint, 12: wireVarint, 13: wireVarint, 14: wireVarint, 15: wireLen, 16: wireLen, 17: wireVarint, 18: wireLen, 19: wireLen, 20: wireLen, 21: wireLen}, 0
	case 3:
		return map[int]int{1: wireLen, 2: wireLen, 3: wireLen, 4: wireVarint, 5: wireLen, 6: wireLen}, 1
	case 4:
		return map[int]int{1: wireLen, 2: wireLen}, 1
	case 5:
		return map[int]int{1: wireLen, 2: wireLen, 3: wireLen, 4: wireLen, 5: wireVarint, 6: wireVarint, 7: wireVarint, 8: wireVarint, 9: wireLen, 10: wireVarint, 11: wireVarint, 12: wireLen, 13: wireVarint, 14: wireLen, 15: wireLen, 16: wireVarint}, 2
	case 7, 29:
		return map[int]int{1: wireLen, 2: wireLen, 4: wireVarint, 5: wireVarint, 6: wireLen}, 1
	case 8:
		return map[int]int{1: wireLen, 2: wireLen, 3: wireLen, 4: wireLen, 5: wireVarint}, 1
	case 9:
		return map[int]int{1: wireLen, 2: wireLen}, 1
	default:
		return nil, 0
	}
}

func encodeAgentClientMessage(id uint32, execID string, hasExecID bool, messageField int, message []byte) []byte {
	execClientMessage := fieldVarint(1, uint64(id))
	if hasExecID {
		execClientMessage = append(execClientMessage, fieldString(15, execID)...)
	}
	execClientMessage = append(execClientMessage, fieldBytes(messageField, message)...)
	return fieldBytes(2, execClientMessage) // AgentClientMessage.exec_client_message
}

func agentMCPRejectedFrame(id uint32, execID string, hasExecID bool, reason string) []byte {
	mcpRejected := concat(fieldString(1, reason), fieldVarint(2, 0))
	mcpResult := fieldBytes(3, mcpRejected)
	return wrapConnectFrame(encodeAgentClientMessage(id, execID, hasExecID, 11, mcpResult))
}

func agentMCPDeniedFrame(id uint32, execID string, hasExecID bool, reason string) []byte {
	mcpResult := fieldBytes(4, fieldString(1, reason))
	return wrapConnectFrame(encodeAgentClientMessage(id, execID, hasExecID, 11, mcpResult))
}

func agentMCPToolNotFoundFrame(id uint32, execID string, hasExecID bool, name string, catalog *agentToolCatalog) []byte {
	var notFound []byte
	if name != "" {
		notFound = append(notFound, fieldString(1, name)...)
	}
	if catalog != nil {
		names := make([]string, 0, len(catalog.tools))
		for toolName := range catalog.tools {
			names = append(names, toolName)
		}
		sort.Strings(names)
		for _, toolName := range names {
			notFound = append(notFound, fieldString(2, toolName)...)
		}
	}
	mcpResult := fieldBytes(5, notFound)
	return wrapConnectFrame(encodeAgentClientMessage(id, execID, hasExecID, 11, mcpResult))
}

const agentInteractionRejectReason = "Interactive Cursor IDE requests are not available through this gateway."

// agentInteractionResponse 应答 AgentServerMessage.interaction_query。Cursor 在收到应答前
// 会一直挂起本轮，所以每个已知询问都必须有回复：托管的 WebSearch/WebFetch 在 Cursor 服务端
// 执行、不触及客户端，予以批准；需要 IDE 交互或有副作用的询问一律拒绝。
// 无法表达拒绝的询问（VM/环境/SCM）返回 ok=false，由调用方以请求错误结束本轮。
func agentInteractionResponse(query []byte) ([]byte, bool) {
	fields, err := decodeAgentProto(query)
	if err != nil {
		return nil, false
	}
	var id uint64
	kind := 0
	for _, field := range fields {
		switch {
		case field.number == 1 && field.wire == wireVarint:
			id = field.value
		case field.number >= 2 && field.wire == wireLen && kind == 0:
			kind = field.number
		}
	}
	rejected := fieldBytes(2, fieldString(1, agentInteractionRejectReason))
	var result []byte
	switch kind {
	case 2, 9: // web_search / web_fetch → approved{}
		result = fieldBytes(1, nil)
	case 4, 11, 12: // switch_mode / mcp_auth / generate_image → rejected{reason}
		result = rejected
	case 3: // ask_question → AskQuestionResult.rejected
		result = fieldBytes(1, fieldBytes(3, fieldString(1, agentInteractionRejectReason)))
	case 7: // create_plan → CreatePlanResult.error
		result = fieldBytes(1, fieldBytes(2, fieldString(1, agentInteractionRejectReason)))
	case 10: // pr_management → PrManagementResult.rejected
		result = fieldBytes(3, fieldString(1, agentInteractionRejectReason))
	default:
		return nil, false
	}
	response := concat(fieldVarint(1, id), fieldBytes(kind, result))
	return wrapConnectFrame(fieldBytes(6, response)), true // AgentClientMessage.interaction_response
}

// validateNativeArgs 只校验已知字段的 wire type：原生工具参数一律被拒绝或按需映射，
// Cursor 新增的字段（如 ShellArgs 23）不应让整轮失败。
func validateNativeArgs(fields []agentProtoField, execField int) error {
	spec, _ := nativeArgsSpec(execField)
	for _, field := range fields {
		if want, known := spec[field.number]; known && field.wire != want {
			return fmt.Errorf("invalid %s field %d wire type", nativeArgsName(execField), field.number)
		}
	}
	return nil
}

// agentExecThrowFrames 以 ExecClientThrow + stream_close 回复无法处理的 exec，
// 与 Cursor 自带执行器对未知消息的处理一致，服务端会把错误交给模型而不是阻塞。
func agentExecThrowFrames(id uint32, message string) []byte {
	throw := fieldBytes(2, concat(fieldVarint(1, uint64(id)), fieldString(2, message)))
	closeStream := fieldBytes(1, fieldVarint(1, uint64(id)))
	return concat(
		wrapConnectFrame(fieldBytes(5, throw)),
		wrapConnectFrame(fieldBytes(5, closeStream)),
	)
}

// agentExecKind 返回 ExecServerMessage 中工具 oneof 的字段号，仅用于统计。
func agentExecKind(exec []byte) int {
	fields, err := decodeAgentProto(exec)
	if err != nil {
		return 0
	}
	for _, field := range fields {
		switch field.number {
		case 1, 15, 19, 55:
		default:
			return field.number
		}
	}
	return 0
}
