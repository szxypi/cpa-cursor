package main

import (
	"bytes"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// checkpoint 同时落盘，CPA 重启或部署后仍可续接；文件含会话内容，仅服务用户可读。
func agentCheckpointDefaultDir() string {
	return filepath.Join(cwdOrRoot(), "cpa-cursor", "checkpoints")
}

var agentCheckpointSaves atomic.Uint64

type agentCheckpointFile struct {
	State          []byte
	Blobs          map[string][]byte
	ConversationID string
	Tools          [32]byte
	Expiry         time.Time
	Binding        [32]byte
	Messages       [][32]byte
	Lengths        []int
	Reply          [32]byte
	Shapes         [][32]byte
}

// enableDisk 开启落盘，并把未过期的 checkpoint 载入内存，使宽松匹配在重启后同样可用。
func (s *agentCheckpointStore) enableDisk() {
	s.mu.Lock()
	if s.disk != "" {
		s.mu.Unlock()
		return
	}
	dir := agentCheckpointDefaultDir()
	s.disk = dir
	s.mu.Unlock()
	go s.loadAllDisk(dir)
}

func (s *agentCheckpointStore) loadAllDisk(dir string) {
	s.pruneDisk(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".ckpt")
		raw, err := hex.DecodeString(name)
		if err != nil || len(raw) != 32 || name == e.Name() {
			continue
		}
		var key [32]byte
		copy(key[:], raw)
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		cp := decodeAgentCheckpoint(data)
		if cp == nil {
			continue
		}
		s.mu.Lock()
		if _, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && s.m[key] == nil && s.now().Before(cp.expiry) && len(s.m) < agentCheckpointLimit {
			s.m[key] = cp
		}
		s.mu.Unlock()
	}
}

func agentCheckpointPath(dir string, key [32]byte) string {
	return filepath.Join(dir, hex.EncodeToString(key[:])+".ckpt")
}

func (s *agentCheckpointStore) persist(dir string, key [32]byte, cp *agentCheckpoint) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(agentCheckpointFile{cp.state, cp.blobs, cp.conversationID, cp.tools, cp.expiry, cp.binding, cp.messages, cp.lengths, cp.reply, cp.shapes}); err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".ckpt-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), agentCheckpointPath(dir, key)) != nil {
		os.Remove(tmp.Name())
		return
	}
	if agentCheckpointSaves.Add(1)%32 == 1 {
		s.pruneDisk(dir)
	}
}

// loadDisk 取出并删除磁盘上的 checkpoint（与内存一样只用一次）；调用方须持有 s.mu。
func (s *agentCheckpointStore) loadDisk(dir string, key [32]byte) *agentCheckpoint {
	path := agentCheckpointPath(dir, key)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	os.Remove(path)
	return decodeAgentCheckpoint(data)
}

func decodeAgentCheckpoint(data []byte) *agentCheckpoint {
	var f agentCheckpointFile
	if gob.NewDecoder(bytes.NewReader(data)).Decode(&f) != nil {
		return nil
	}
	return &agentCheckpoint{state: f.State, blobs: f.Blobs, conversationID: f.ConversationID, tools: f.Tools, expiry: f.Expiry, binding: f.Binding, messages: f.Messages, lengths: f.Lengths, reply: f.Reply, shapes: f.Shapes}
}

func (s *agentCheckpointStore) removeDisk(dir string, key [32]byte) {
	os.Remove(agentCheckpointPath(dir, key))
}

func (s *agentCheckpointStore) pruneDisk(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := s.now().Add(-agentCheckpointTTL)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
