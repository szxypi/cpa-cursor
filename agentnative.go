package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// agentNativeHandoffMessage 作为被映射的原生工具的即时结果：调用已交给客户端的同类工具执行，
// 本次 run 随即结束，真实结果随下一次请求发送。
func agentNativeHandoffMessage(tool string) string {
	return fmt.Sprintf("This call was handed off to the client's %s tool for execution. End your turn immediately without further text; the actual result will arrive in the next message.", tool)
}

// mapNativeExec 把 Cursor 原生 Shell/Grep/Write 映射为调用方声明的同类工具（如 Claude Code 的
// Bash/Grep/Write）。只有 schema 能证明参数一一对应时才映射；否则返回 nil，由上层照常拒绝。
// 插件本身不执行任何命令或文件操作。
func (c *agentToolCatalog) mapNativeExec(id uint32, execID string, hasExecID bool, execField agentProtoField, fields []agentProtoField) *agentToolRequest {
	if c == nil {
		return nil
	}
	values, err := agentSingularFields(fields, map[int]bool{5: execField.number == 2 || execField.number == 14, 22: true})
	if err != nil {
		return nil
	}
	str := func(n int) string {
		if v, ok := values[n]; ok && v.wire == wireLen && utf8.Valid(v.bytes) {
			return string(v.bytes)
		}
		return ""
	}
	integer := func(n int) (json.Number, bool) {
		v, ok := values[n]
		if !ok || v.wire != wireVarint {
			return "", false
		}
		return json.Number(strconv.FormatInt(int64(int32(v.value)), 10)), true
	}
	boolean := func(n int) (bool, bool) {
		v, ok := values[n]
		return v.value != 0, ok && v.wire == wireVarint
	}

	var names []string
	required := map[string]any{}
	optional := map[string]any{}
	switch execField.number {
	case 2, 14:
		command := strings.TrimSpace(str(1))
		if command == "" {
			return nil
		}
		if dir := str(2); dir != "" {
			command = "cd " + agentShellQuote(dir) + " && " + command
		}
		names = []string{"Bash", "bash", "Shell", "shell"}
		required["command"] = command
		if description := str(15); description != "" {
			optional["description"] = description
		}
		if background, ok := boolean(11); ok && background {
			optional["run_in_background"] = true
		}
	case 5:
		pattern := str(1)
		if pattern == "" {
			return nil
		}
		names = []string{"Grep", "grep"}
		required["pattern"] = pattern
		for n, name := range map[int]string{2: "path", 3: "glob", 4: "output_mode", 9: "type"} {
			if v := str(n); v != "" {
				optional[name] = v
			}
		}
		for n, name := range map[int]string{5: "-B", 6: "-A", 7: "-C", 10: "head_limit", 16: "offset"} {
			if v, ok := integer(n); ok {
				optional[name] = v
			}
		}
		if v, ok := boolean(8); ok && v {
			optional["-i"] = true
		}
		if v, ok := boolean(11); ok && v {
			optional["multiline"] = true
		}
	case 3:
		path := str(1)
		if path == "" || len(values[5].bytes) > 0 {
			return nil
		}
		names = []string{"Write", "write"}
		required["file_path"] = path
		required["content"] = str(2)
	default:
		return nil
	}
	if request := c.mapNativeTo(id, execID, hasExecID, execField, names, required, optional); request != nil {
		return request
	}
	if execField.number == 5 {
		return c.mapNativeTo(id, execID, hasExecID, execField, []string{"Bash", "bash", "Shell", "shell"},
			map[string]any{"command": agentGrepCommand(required["pattern"].(string), optional)},
			map[string]any{"description": "Search file contents with ripgrep"})
	}
	return nil
}

func (c *agentToolCatalog) mapNativeTo(id uint32, execID string, hasExecID bool, execField agentProtoField, names []string, required, optional map[string]any) *agentToolRequest {
	for _, name := range names {
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
		properties, _ := root["properties"].(map[string]any)
		arguments := make(map[string]any, len(required)+len(optional))
		for k, v := range required {
			arguments[k] = v
		}
		if execField.number == 3 {
			if _, ok := properties["file_path"]; !ok {
				if _, ok := properties["path"]; ok {
					delete(arguments, "file_path")
					arguments["path"] = required["file_path"]
				}
			}
		}
		for k, v := range optional {
			if _, declared := properties[k]; declared {
				arguments[k] = v
			}
		}
		if !agentSchemaRequiredPropertiesMapped(root, arguments) || !agentSchemaOnlyAllowsMappedProperties(root, arguments) || !agentSchemaTypesAccept(root, arguments) {
			continue
		}
		argumentJSON, err := json.Marshal(arguments)
		if err != nil || len(argumentJSON) > maxAgentToolArgsBytes {
			continue
		}
		handoff := nativeToolRejectionFrame(id, execID, hasExecID, execField, agentNativeHandoffMessage(name))
		if handoff == nil {
			return nil
		}
		return &agentToolRequest{
			Name:      name,
			Arguments: string(argumentJSON),
			id:        id,
			execID:    execID,
			hasExecID: hasExecID,
			provider:  "cpa-client",
			toolName:  name,
			handoff:   handoff,
		}
	}
	return nil
}

// agentGrepCommand 把原生 grep 参数还原为等价的 rg 命令，供只声明了 Bash 的客户端执行。
func agentGrepCommand(pattern string, options map[string]any) string {
	args := []string{"rg"}
	switch options["output_mode"] {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	default:
		args = append(args, "-n")
	}
	for _, flag := range []string{"-B", "-A", "-C"} {
		if v, ok := options[flag].(json.Number); ok {
			args = append(args, flag, v.String())
		}
	}
	if options["-i"] == true {
		args = append(args, "-i")
	}
	if options["multiline"] == true {
		args = append(args, "-U", "--multiline-dotall")
	}
	if v, ok := options["glob"].(string); ok {
		args = append(args, "--glob", agentShellQuote(v))
	}
	if v, ok := options["type"].(string); ok {
		args = append(args, "--type", agentShellQuote(v))
	}
	args = append(args, "-e", agentShellQuote(pattern))
	if v, ok := options["path"].(string); ok {
		args = append(args, "--", agentShellQuote(v))
	}
	command := strings.Join(args, " ")
	if v, ok := options["offset"].(json.Number); ok && v.String() != "0" {
		if n, err := v.Int64(); err == nil && n > 0 {
			command += " | tail -n +" + strconv.FormatInt(n+1, 10)
		}
	}
	if v, ok := options["head_limit"].(json.Number); ok {
		if n, err := v.Int64(); err == nil && n > 0 {
			command += " | head -n " + strconv.FormatInt(n, 10)
		}
	}
	return command
}

// nativeCandidates 概述客户端声明的同类工具及其参数名，用于排查原生工具未能映射的原因。
func (c *agentToolCatalog) nativeCandidates() string {
	var parts []string
	for _, name := range []string{"Bash", "bash", "Shell", "shell", "Grep", "grep", "Write", "write"} {
		tool, ok := c.tools[name]
		if !ok {
			continue
		}
		var props []string
		if schema, err := decodeAgentJSON(tool.Arguments); err == nil {
			if root, ok := schema.(map[string]any); ok {
				properties, _ := root["properties"].(map[string]any)
				for k := range properties {
					props = append(props, k)
				}
				if required, ok := root["required"].([]any); ok {
					props = append(props, fmt.Sprintf("required=%v", required))
				}
			}
		}
		sort.Strings(props)
		parts = append(parts, name+"("+strings.Join(props, ",")+")")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func agentShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
