package main

import (
	"crypto/sha256"
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
}

type agentCheckpointStore struct {
	mu  sync.Mutex
	m   map[[32]byte]*agentCheckpoint
	now func() time.Time
}

var agentCheckpoints = &agentCheckpointStore{m: make(map[[32]byte]*agentCheckpoint), now: time.Now}

func agentCheckpointKey(binding [32]byte, prefix []openAIMessage, assistantText string) [32]byte {
	history := agentHistoryHash(prefix)
	h := sha256.New()
	h.Write(binding[:])
	h.Write(history[:])
	h.Write([]byte(strings.TrimSpace(assistantText)))
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

// save 记录以纯文本回复结束的一轮；prefix 是该轮请求的完整历史，客户端下一条请求会在
// 其后追加这条回复和新的用户消息。
func (s *agentCheckpointStore) save(p *preparedChat, client *agentClient, assistantText string) {
	if p == nil || client == nil || len(client.checkpoint) == 0 || client.context == nil || strings.TrimSpace(assistantText) == "" {
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
	}
	key := agentCheckpointKey(p.binding, p.parsed.RawMessages, assistantText)
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

// take 取出与本次请求历史精确对应的 checkpoint，并返回需要作为新用户消息发送的文本。
// 历史被改写、工具集变化或最后一条不是纯文本回复时返回 nil，调用方照常带完整历史重开。
func (s *agentCheckpointStore) take(p *preparedChat) (*agentCheckpoint, string) {
	ms := p.parsed.RawMessages
	start := agentTrailingStart(ms)
	if start < 1 || start == len(ms) {
		return nil, ""
	}
	assistant := ms[start-1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) > 0 {
		return nil, ""
	}
	var texts []string
	for _, m := range ms[start:] {
		if text := strings.TrimSpace(contentText(m.Content)); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return nil, ""
	}
	key := agentCheckpointKey(p.binding, ms[:start-1], contentText(assistant.Content))
	s.mu.Lock()
	cp := s.m[key]
	delete(s.m, key)
	s.mu.Unlock()
	if cp == nil || !s.now().Before(cp.expiry) || cp.tools != agentToolsHash(p.parsed.Tools) {
		return nil, ""
	}
	return cp, strings.Join(texts, "\n\n")
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
