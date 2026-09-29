package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	agentCheckpointTTL   = 30 * time.Minute
	agentCheckpointLimit = 256
)

// agentCheckpoint 是一轮结束时 Cursor 下发的会话状态及其引用的 blob。下一条用户消息
// 只需携带它和新文本即可续接，服务端不必重新处理整段历史。checkpoint 与账号绑定，
// 也只对产生它的那段客户端历史有效，因此以调用方身份 + 历史哈希 + 最终回复为键。
type agentCheckpoint struct {
	state          []byte
	blobs          map[string][]byte
	conversationID string
	tools          [32]byte
	expiry         time.Time
	// 以下仅用于诊断未命中原因，不参与匹配。
	binding  [32]byte
	messages [][32]byte
	reply    [32]byte
}

type agentCheckpointStore struct {
	mu  sync.Mutex
	m   map[[32]byte]*agentCheckpoint
	now func() time.Time
}

var agentCheckpoints = &agentCheckpointStore{m: make(map[[32]byte]*agentCheckpoint), now: time.Now}

// agentAssistantMessage 以客户端视角还原本轮回复，用于匹配客户端下一次回传的历史。
func agentAssistantMessage(text string, calls []openAIToolCall) openAIMessage {
	content, _ := json.Marshal(text)
	return openAIMessage{Role: "assistant", Content: content, ToolCalls: calls}
}

func agentCheckpointKey(binding [32]byte, prefix []openAIMessage, assistant openAIMessage) [32]byte {
	// 客户端格式转换会把工具调用前后的文字拆成多个文本块再以换行拼回，比较时忽略空白。
	content, _ := json.Marshal(strings.Join(strings.Fields(contentText(assistant.Content)), ""))
	normalized := openAIMessage{Role: "assistant", Content: content}
	for _, call := range assistant.ToolCalls {
		c := openAIToolCall{ID: call.ID, Type: "function"}
		c.Function.Name = call.Function.Name
		c.Function.Arguments = call.Function.Arguments
		normalized.ToolCalls = append(normalized.ToolCalls, c)
	}
	history := agentHistoryHash(prefix)
	reply := agentHistoryHash([]openAIMessage{normalized})
	h := sha256.New()
	h.Write(binding[:])
	h.Write(history[:])
	h.Write(reply[:])
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

// save 记录正常结束的一轮；prefix 是该轮请求的完整历史，客户端下一次请求会在其后追加
// 这条回复、工具结果和新的用户消息。
func (s *agentCheckpointStore) save(p *preparedChat, client *agentClient, assistant openAIMessage) {
	if p == nil || client == nil || client.context == nil {
		return
	}
	if len(client.checkpoint) == 0 {
		p.stats.setCheckpoint("none")
		return
	}
	p.stats.setCheckpoint("saved")
	if strings.TrimSpace(contentText(assistant.Content)) == "" && len(assistant.ToolCalls) == 0 {
		return
	}
	blobs := make(map[string][]byte, len(client.context.blobs))
	for k, v := range client.context.blobs {
		blobs[k] = v
	}
	cp := &agentCheckpoint{
		state:          client.checkpoint,
		blobs:          blobs,
		conversationID: client.context.conversationID,
		tools:          agentToolsHash(p.parsed.Tools),
		binding:        p.binding,
		messages:       agentMessageHashes(p.parsed.RawMessages),
		reply:          agentCheckpointKey([32]byte{}, nil, assistant),
	}
	key := agentCheckpointKey(p.binding, p.parsed.RawMessages, assistant)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	cp.expiry = now.Add(agentCheckpointTTL)
	for k, v := range s.m {
		if !now.Before(v.expiry) {
			delete(s.m, k)
		}
	}
	if len(s.m) >= agentCheckpointLimit {
		var oldest [32]byte
		var oldestAt time.Time
		for k, v := range s.m {
			if oldestAt.IsZero() || v.expiry.Before(oldestAt) {
				oldest, oldestAt = k, v.expiry
			}
		}
		delete(s.m, oldest)
	}
	s.m[key] = cp
}

// take 取出与本次请求历史精确对应的 checkpoint，并返回作为新用户消息发送的文本：上一轮
// 交给客户端的工具结果，加上之后的用户消息。无法续接时返回原因，调用方带完整历史重开。
func (s *agentCheckpointStore) take(p *preparedChat) (*agentCheckpoint, string, string) {
	ms := p.parsed.RawMessages
	start := agentTrailingStart(ms)
	i := start
	for i > 0 && ms[i-1].Role == "tool" {
		i--
	}
	a := i - 1
	if a < 0 {
		return nil, "", "no_history"
	}
	assistant := ms[a]
	if assistant.Role != "assistant" {
		return nil, "", "last:" + assistant.Role
	}
	var texts []string
	if len(assistant.ToolCalls) > 0 {
		results := make(map[string]openAIMessage, start-i)
		for _, m := range ms[i:start] {
			results[m.ToolCallID] = m
		}
		if len(results) != len(assistant.ToolCalls) {
			return nil, "", "tool_results"
		}
		var b strings.Builder
		b.WriteString("Results of the tool calls handed off in the previous turn:")
		for _, call := range assistant.ToolCalls {
			result, ok := results[call.ID]
			if !ok {
				return nil, "", "tool_results"
			}
			status := ""
			if p.toolErrors[call.ID] {
				status = ` is_error="true"`
			}
			fmt.Fprintf(&b, "\n\n<tool_result name=%q id=%q%s>\n%s\n</tool_result>", call.Function.Name, call.ID, status, contentText(result.Content))
		}
		texts = append(texts, b.String())
	} else if i != start {
		return nil, "", "tool_results"
	}
	for _, m := range ms[start:] {
		if text := strings.TrimSpace(contentText(m.Content)); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return nil, "", "no_new_input"
	}
	key := agentCheckpointKey(p.binding, ms[:a], assistant)
	s.mu.Lock()
	cp := s.m[key]
	delete(s.m, key)
	s.mu.Unlock()
	switch {
	case cp == nil:
		return nil, "", s.diagnose(p.binding, ms[:a], assistant)
	case !s.now().Before(cp.expiry):
		return nil, "", "expired"
	case cp.tools != agentToolsHash(p.parsed.Tools):
		return nil, "", "tools"
	}
	return cp, strings.Join(texts, "\n\n"), ""
}

// apply 让新 run 以 checkpoint 为会话状态、只发送新用户消息；本次请求的规则与工具仍由
// requestContext 实时提供。
func (cp *agentCheckpoint) apply(c *agentContext, text string) {
	for k, v := range cp.blobs {
		if _, exists := c.blobs[k]; !exists {
			c.blobs[k] = v
		}
	}
	c.state = cp.state
	c.conversationID = cp.conversationID
	c.resumeText = text
}

func (s *agentCheckpointStore) reset() {
	s.mu.Lock()
	s.m = make(map[[32]byte]*agentCheckpoint)
	s.mu.Unlock()
}

func agentMessageHashes(ms []openAIMessage) [][32]byte {
	out := make([][32]byte, len(ms))
	for i := range ms {
		out[i] = agentHistoryHash(ms[i : i+1])
	}
	return out
}

// diagnose 在同一调用方的 checkpoint 中找与本次历史最接近的一条，说明未命中的位置：
// prefix@i:role 表示第 i 条历史被改写，reply 表示上一条回复与交付内容不同。
func (s *agentCheckpointStore) diagnose(binding [32]byte, prefix []openAIMessage, assistant openAIMessage) string {
	current := agentMessageHashes(prefix)
	reply := agentCheckpointKey([32]byte{}, nil, assistant)
	s.mu.Lock()
	defer s.mu.Unlock()
	best, reason := -1, "no_checkpoint"
	for _, cp := range s.m {
		if cp.binding != binding {
			continue
		}
		n := 0
		for n < len(cp.messages) && n < len(current) && cp.messages[n] == current[n] {
			n++
		}
		if n <= best {
			continue
		}
		best = n
		switch {
		case n == len(cp.messages) && n == len(current):
			reason = "reply"
			if cp.reply == reply {
				reason = "reply_same"
			}
		case n < len(cp.messages) && n < len(current):
			reason = fmt.Sprintf("prefix@%d/%d:%s", n, len(current), prefix[n].Role)
		default:
			reason = fmt.Sprintf("prefix_len:%d/%d", len(current), len(cp.messages))
		}
	}
	if best < 0 {
		return "no_checkpoint(binding)"
	}
	return reason
}
