package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// 字段依据 agent.v1 的生成 schema 和 oh-my-pi Cursor 客户端。
// 系统指令走全局规则；会话历史走 root_prompt_messages_json 的 blob 引用。
// 两者都不能由团队专用的 custom_system_prompt（field 8）替代。
// https://github.com/leookun/cursor-byok/blob/9a8fde279c701b47dae97b7a18ebe92fa0119013/protocols/cursor/agent_v1.proto
// https://github.com/can1357/oh-my-pi/blob/cbe9bc206d07c4395ff465623e229d279d0dd437/packages/ai/src/providers/cursor.ts
type agentContext struct {
	state []byte
	rules []byte
	blobs map[string][]byte
	tools *agentToolCatalog
	// conversationID 在首次构造 run 帧时生成；resumeText 非空表示 state 来自 checkpoint，
	// run 只携带这段新用户文本。
	conversationID string
	resumeText     string
}

func newAgentContext(messages []cursorMessage) *agentContext {
	c := &agentContext{blobs: make(map[string][]byte)}
	lastUser := -1
	for i, m := range messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	systemCount := 0
	var instructions []string
	for _, m := range messages {
		if m.Role != "system" || m.Content == "" {
			continue
		}
		rule := concat(fieldString(1, fmt.Sprintf("/cpa/system/%d.mdc", systemCount)), fieldString(2, m.Content), fieldBytes(3, fieldBytes(1, nil)), fieldVarint(4, 2))
		c.rules = append(c.rules, fieldBytes(2, rule)...)
		instructions = append(instructions, m.Content)
		c.addMessage(map[string]any{"role": "system", "content": m.Content})
		systemCount++
	}
	if systemCount == 0 {
		c.addMessage(map[string]any{"role": "system", "content": "You are a helpful assistant."})
	} else {
		// 后端重建多轮 prompt 时可能丢弃 system/rules；保留有明确来源的内容副本。
		// 此副本是 user 角色，不宣称等价于原生 system 优先级。
		encoded, _ := json.Marshal(instructions)
		copyText := "Application instructions (compatibility copy; message role remains user):\n" + string(encoded)
		c.addMessage(map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": copyText}}})
	}
	for i, m := range messages {
		if i == lastUser {
			break
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		c.addMessage(map[string]any{"role": m.Role, "content": []map[string]string{{"type": "text", "text": m.Content}}})
	}
	return c
}
func (c *agentContext) addMessage(v any) {
	data, _ := json.Marshal(v)
	hash := sha256.Sum256(data)
	c.blobs[string(hash[:])] = data
	c.state = append(c.state, fieldBytes(1, hash[:])...)
}

func (c *agentContext) contextResponse(exec []byte) []byte {
	var rules []byte
	if c != nil {
		rules = c.rules
		if c.tools != nil {
			rules = concat(rules, c.tools.contextFields())
		}
	}
	success := fieldBytes(1, rules)
	response := fieldBytes(10, fieldBytes(1, success))
	// 回传请求关联字段，不向客户端日志暴露它们。
	for _, f := range decodeMessage(exec) {
		if f.Number == 1 || f.Number == 15 {
			if f.IsLen {
				response = append(response, fieldBytes(f.Number, f.Value)...)
			} else {
				response = append(response, fieldVarint(f.Number, f.Varint)...)
			}
		}
	}
	return wrapConnectFrame(fieldBytes(2, response))
}

func (c *agentContext) kvResponse(kv []byte) ([]byte, error) {
	fields := decodeMessage(kv)
	id, _ := fieldFirst(fields, 1)
	response := fieldVarint(1, id.Varint)
	if get, ok := fieldFirst(fields, 2); ok && get.IsLen {
		blobID, _ := fieldFirst(decodeMessage(get.Value), 1)
		var result []byte
		if data, found := c.blobs[string(blobID.Value)]; found {
			result = fieldBytes(1, data)
		}
		response = append(response, fieldBytes(2, result)...)
	} else if set, ok := fieldFirst(fields, 3); ok && set.IsLen {
		parts := decodeMessage(set.Value)
		key, _ := fieldFirst(parts, 1)
		data, _ := fieldFirst(parts, 2)
		// 每条请求独立存储，并复制字节，避免持有网络缓冲区。
		c.blobs[string(key.Value)] = append([]byte(nil), data.Value...)
		response = append(response, fieldBytes(3, nil)...)
	} else {
		return nil, fmt.Errorf("unsupported Cursor KV message")
	}
	return wrapConnectFrame(fieldBytes(3, response)), nil
}
