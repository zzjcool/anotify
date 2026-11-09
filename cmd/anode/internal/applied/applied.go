// Package applied 读写 `anode.applied.yaml`（D-I4 / config.md §4.2）。
//
// 文件在 dataDir 下，0600，写临时文件 + rename 原子落盘；内容是**机器级合并
// 结果**（local ⊕ node）：`{revision,hash,vars,limits}`。namespace 默认值只在
// 任务内存里活一次，绝不写进这个文件。
//
// 读是 strict 的：未知字段失败、非 JSON 失败、hash 与内容不一致失败（手改/损坏
// 都判 drift，不把被改过的值当生效配置）。文件不存在是冷启动（Present=false），
// 不是错误，也绝不伪造「上次在线」的 revision/hash。
package applied

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zzjcool/anotify/internal/protocol"
)

// ErrNotExist 是「还没有 applied 文件」（冷启动）。
var ErrNotExist = errors.New("applied: 文件不存在")

// Store 是 applied 文件的读写入口。
type Store struct {
	path string
}

// NewStore 在 dataDir 下定位 `anode.applied.yaml`。
func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, protocol.AppliedFileName)}
}

// Path 返回 applied 文件路径。
func (s *Store) Path() string { return s.path }

// Load 读取并校验 applied 文件。
//
// 返回 Present=false（且 err=nil）表示冷启动。错误返回时调用方**不得**把
// 零值当成有效配置：必须按「没有 applied」处理并等待中枢 welcome 重新对齐。
func (s *Store) Load() (protocol.AppliedConfig, bool, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return protocol.AppliedConfig{}, false, nil
		}
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: 查看 %s: %w", s.path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: %s 是符号链接，拒绝读取", s.path)
	}
	if !info.Mode().IsRegular() {
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: %s 不是普通文件", s.path)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: 读取 %s: %w", s.path, err)
	}
	cfg, err := decodeStrict(data)
	if err != nil {
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: %s: %w", s.path, err)
	}
	hash, err := protocol.MachineConfigHash(cfg.Vars, cfg.Limits)
	if err != nil {
		return protocol.AppliedConfig{}, false, fmt.Errorf("applied: %s: %w", s.path, err)
	}
	if hash != cfg.Hash {
		return protocol.AppliedConfig{}, false, fmt.Errorf(
			"applied: %s hash 校验失败：文件 %s，按内容重算 %s（文件被改过或损坏）",
			s.path, cfg.Hash, hash)
	}
	return cfg, true, nil
}

// decodeStrict 严格解码 applied JSON：未知字段、尾随内容都失败。
func decodeStrict(data []byte) (protocol.AppliedConfig, error) {
	var cfg protocol.AppliedConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return protocol.AppliedConfig{}, fmt.Errorf("解码: %w", err)
	}
	var extra any
	err := dec.Decode(&extra)
	if err == nil {
		return protocol.AppliedConfig{}, errors.New("解码: 文件含多个 JSON 值")
	}
	if !errors.Is(err, io.EOF) {
		return protocol.AppliedConfig{}, fmt.Errorf("解码: %w", err)
	}
	return cfg, nil
}

// Save 计算 hash、原子写入并返回落盘内容。
//
// 顺序很关键：先写成功才返回 cfg；调用方只有拿到 cfg 才能发 config.applied
// （config.md §4.2「写成功前不切生效值」）。任何一步失败都不留半份文件。
func (s *Store) Save(revision string, vars map[string]string, limits protocol.Limits) (protocol.AppliedConfig, error) {
	hash, err := protocol.MachineConfigHash(vars, limits)
	if err != nil {
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 计算 hash: %w", err)
	}
	cfg := protocol.AppliedConfig{
		Revision: revision,
		Hash:     hash,
		Vars:     cloneVars(vars),
		Limits:   cloneLimits(limits),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 编码: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 创建目录 %s: %w", dir, err)
	}
	// 已有路径是目录/符号链接时，rename 替换的语义不符合预期；先拒绝。
	if info, statErr := os.Lstat(s.path); statErr == nil && info.IsDir() {
		return protocol.AppliedConfig{}, fmt.Errorf("applied: %s 是目录，拒绝覆盖", s.path)
	}
	tmp, err := os.CreateTemp(dir, "."+protocol.AppliedFileName+".tmp-*")
	if err != nil {
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 设置权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 写入: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return protocol.AppliedConfig{}, fmt.Errorf("applied: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return protocol.AppliedConfig{}, fmt.Errorf("applied: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return protocol.AppliedConfig{}, fmt.Errorf("applied: rename: %w", err)
	}
	// 目录 fsync 是尽力而为：失败不影响 rename 已生效的原子性。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return cfg, nil
}

func cloneVars(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return out
}

func cloneLimits(l protocol.Limits) protocol.Limits {
	out := protocol.Limits{Total: l.Total}
	if len(l.PerCapability) > 0 {
		out.PerCapability = make(map[string]int, len(l.PerCapability))
		for k, v := range l.PerCapability {
			out.PerCapability[k] = v
		}
	}
	return out
}
