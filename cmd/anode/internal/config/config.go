// Package config 解析并校验 anode.yaml（anode.md §1/§2/§5，config.md §4.5）。
//
// 启动是**全有或全无**：文件不存在、有未知字段（含嵌套）、身份字段缺失或不合
// 格式、namespaces 显式为空/重复、vars 名/值不合规、limits 非正、envPassthrough
// 名单违规——任何一项都返回汇总错误，不做「跳过坏的、保留好的」。
//
// 本包只回答「本机配置是什么」；机器级合并（local > node）与 hash 在 merge.go，
// 生效落盘在 internal/applied，配置帧流程在 internal/hub。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzjcool/anotify/cmd/anode/internal/listen"
	"github.com/zzjcool/anotify/internal/protocol"
)

const (
	// DefaultNamespace 是 namespaces 缺省值（anode.md §0）。
	DefaultNamespace = "default"
	// Wildcard 是「全部命名空间」的字面值。
	Wildcard = "*"
	// MaxVarsValueBytes 是 vars 值的字节上限（anode.md §3.1）。
	MaxVarsValueBytes = 4096
	// MaxPassthroughNames 是 envPassthrough 的名单上限（anode.md §2）。
	MaxPassthroughNames = 32
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	varPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Error 是一组校验问题的汇总（启动期逐项列出，不半在线）。
type Error struct {
	Problems []string
}

func (e *Error) Error() string { return strings.Join(e.Problems, "; ") }

// Config 是 anode.yaml 的本机配置（字段与 anode.md §2 表格一一对应）。
type Config struct {
	// Name 是节点名（集群内唯一；换名字 = 换一台机器）。
	Name string `yaml:"name"`
	// Server 是中枢地址（ws:// 或 wss://；空 path 时客户端补 /v1/node/stream）。
	Server string `yaml:"server"`
	// Token 是节点身份令牌（不绑 ns）。
	Token string `yaml:"token"`
	// Namespaces 是本机**申报**列表；缺省 ["default"]，显式 [] 与重复都报错。
	Namespaces []string `yaml:"namespaces"`
	// Listen 是 emit 端点（默认 tcp://127.0.0.1:0）。
	Listen string `yaml:"listen"`
	// Vars 是机器级本机值（同键压住中枢节点默认值 / ns 默认值）。
	Vars map[string]string `yaml:"vars"`
	// Limits 是本机并发预算（total 未写与显式 0 是两件事）。
	Limits Limits `yaml:"limits"`
	// EnvPassthrough 是凭证变量**名字**白名单（值留在本机，不上报）。
	EnvPassthrough []string `yaml:"envPassthrough"`
}

// Limits 是 anode.yaml 里的 limits。
//
// Total 用指针：**未写**（nil）表示「不压住节点默认值」，显式 0 是非法值必须
// 启动失败——两者在合并语义上完全不同（config.md §4.1 / anode.md §2）。
type Limits struct {
	Total         *int           `yaml:"total"`
	PerCapability map[string]int `yaml:"perCapability"`
}

// Load 读取并严格解析 anode.yaml。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse 严格解析 YAML：未知字段（含嵌套）失败、多文档失败、再做完整校验。
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw Config
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{Problems: []string{"anode.yaml 为空"}}
		}
		return nil, fmt.Errorf("解析 anode.yaml: %w", err)
	}
	var extra yaml.Node
	err := dec.Decode(&extra)
	switch {
	case err == nil:
		return nil, &Error{Problems: []string{"anode.yaml 只允许一个 YAML 文档"}}
	case errors.Is(err, io.EOF):
		// 正常结束。
	default:
		return nil, fmt.Errorf("解析 anode.yaml: %w", err)
	}
	if err := raw.normalizeAndValidate(); err != nil {
		return nil, err
	}
	return &raw, nil
}

// normalizeAndValidate 补缺省并逐项校验（问题一次性汇总）。
func (c *Config) normalizeAndValidate() error {
	var p []string

	// name：必填 + 四主命名正则。
	switch {
	case strings.TrimSpace(c.Name) == "":
		p = append(p, "name 必填")
	case !namePattern.MatchString(c.Name):
		p = append(p, fmt.Sprintf("name %q 不合法（%s）", c.Name, namePattern.String()))
	}

	// server：必填 + ws/wss + host。
	if strings.TrimSpace(c.Server) == "" {
		p = append(p, "server 必填")
	} else if u, err := url.Parse(c.Server); err != nil {
		p = append(p, fmt.Sprintf("server %q 不是合法 URL：%v", c.Server, err))
	} else if u.Scheme != "ws" && u.Scheme != "wss" {
		p = append(p, fmt.Sprintf("server %q 必须是 ws:// 或 wss://", c.Server))
	} else if u.Host == "" {
		p = append(p, fmt.Sprintf("server %q 缺少主机名", c.Server))
	} else if u.User != nil {
		p = append(p, "server 不允许在 URL 里带凭据（令牌走 hello / Authorization 头）")
	}

	// token：必填（非空白）。
	if strings.TrimSpace(c.Token) == "" {
		p = append(p, "token 必填")
	}

	// namespaces：缺省 [default]；显式空 / 重复 / 非法元素都失败。
	if c.Namespaces == nil {
		c.Namespaces = []string{DefaultNamespace}
	}
	if len(c.Namespaces) == 0 {
		p = append(p, "namespaces 显式为空（缺省是 [default]；要取消全部授权请删掉字段并接受缺省）")
	}
	seenNS := make(map[string]bool, len(c.Namespaces))
	for _, ns := range c.Namespaces {
		if ns != Wildcard && !namePattern.MatchString(ns) {
			p = append(p, fmt.Sprintf("namespaces 元素 %q 不合法（%s 或 %q）", ns, namePattern.String(), Wildcard))
			continue
		}
		if seenNS[ns] {
			p = append(p, fmt.Sprintf("namespaces 元素 %q 重复", ns))
		}
		seenNS[ns] = true
	}

	// listen：空 → 默认；否则按 listen 包校验。
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = listen.DefaultSpec
	}
	if _, err := listen.Parse(c.Listen); err != nil {
		p = append(p, err.Error())
	}

	// vars：名 / 值。
	p = append(p, validateVars(c.Vars)...)

	// limits：total 显式 0 是非法（未写才是 nil）；perCapability 逐项正数。
	p = append(p, validateLimits(c.Limits)...)

	// envPassthrough：名字 / 数量 / 重复 / 禁用项。
	p = append(p, validatePassthrough(c.EnvPassthrough)...)

	if len(p) > 0 {
		return &Error{Problems: p}
	}
	return nil
}

// validateVars 校验 vars 的名字与值（anode.md §3.1）。
func validateVars(vars map[string]string) []string {
	var p []string
	for name, value := range vars {
		if !varPattern.MatchString(name) {
			p = append(p, fmt.Sprintf("vars 名 %q 不合法（%s）", name, varPattern.String()))
		}
		if value == "" {
			p = append(p, fmt.Sprintf("vars.%s 值不能为空", name))
		}
		if len(value) > MaxVarsValueBytes {
			p = append(p, fmt.Sprintf("vars.%s 值 %d 字节，超过上限 %d", name, len(value), MaxVarsValueBytes))
		}
		if strings.ContainsRune(value, 0) {
			p = append(p, fmt.Sprintf("vars.%s 值含 NUL", name))
		}
	}
	return p
}

// validateLimits 校验本机 limits：total 显式时必须 ≥1；perCapability 键是能力名或 "*"、值 ≥1。
func validateLimits(l Limits) []string {
	var p []string
	if l.Total != nil && *l.Total < 1 {
		p = append(p, fmt.Sprintf("limits.total 必须 ≥1（未写才是缺省 1），实际 %d", *l.Total))
	}
	for name, v := range l.PerCapability {
		if name != Wildcard && !namePattern.MatchString(name) {
			p = append(p, fmt.Sprintf("limits.perCapability 键 %q 不合法（能力名或 %q）", name, Wildcard))
		}
		if v < 1 {
			p = append(p, fmt.Sprintf("limits.perCapability[%s] 必须 ≥1，实际 %d", name, v))
		}
	}
	return p
}

// validatePassthrough 校验 envPassthrough 名单（anode.md §2 / §3.2）。
func validatePassthrough(names []string) []string {
	var p []string
	if len(names) > MaxPassthroughNames {
		p = append(p, fmt.Sprintf("envPassthrough 最多 %d 项，实际 %d", MaxPassthroughNames, len(names)))
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !envPattern.MatchString(name) {
			p = append(p, fmt.Sprintf("envPassthrough 名字 %q 不合法（%s）", name, envPattern.String()))
		}
		if seen[name] {
			p = append(p, fmt.Sprintf("envPassthrough 名字 %q 重复", name))
		}
		seen[name] = true
		switch {
		case name == "PATH":
			p = append(p, "envPassthrough 不允许 PATH（任务 PATH 固定为守护进程启动基线）")
		case name == "HOME":
			p = append(p, "envPassthrough 不允许 HOME（HOME 取 status.homeDir 或 run.env 显式值）")
		case strings.HasPrefix(name, "ANOTIFY_"):
			p = append(p, fmt.Sprintf("envPassthrough 不允许 ANOTIFY_ 前缀（%q）", name))
		}
	}
	return p
}

// ValidateNodeDefaults 校验中枢下发的节点默认值（welcome.nodeVars / nodeLimits）。
//
// 中枢侧写入期已经校验过，这里是节点的防御：非法默认值不参与合并、不落盘。
// total=0 表示「中枢没有设 total」（缺省），允许；perCapability 的值必须 ≥1。
func ValidateNodeDefaults(vars map[string]string, limits protocol.Limits) error {
	p := validateVars(vars)
	if limits.Total < 0 {
		p = append(p, fmt.Sprintf("nodeLimits.total 不能为负：%d", limits.Total))
	}
	for name, v := range limits.PerCapability {
		if name != Wildcard && !namePattern.MatchString(name) {
			p = append(p, fmt.Sprintf("nodeLimits.perCapability 键 %q 不合法", name))
		}
		if v < 1 {
			p = append(p, fmt.Sprintf("nodeLimits.perCapability[%s] 必须 ≥1，实际 %d", name, v))
		}
	}
	if len(p) > 0 {
		return &Error{Problems: p}
	}
	return nil
}

// DeclaredNamespaces 返回本机申报列表的拷贝（已校验、非空）。
func (c *Config) DeclaredNamespaces() []string {
	out := make([]string, len(c.Namespaces))
	copy(out, c.Namespaces)
	return out
}

// LocalLimits 把本机 limits 转成 wire 形状：未写的 total 是 0（协议里 0 = 未设置）。
func (c *Config) LocalLimits() protocol.Limits {
	out := protocol.Limits{}
	if c.Limits.Total != nil {
		out.Total = *c.Limits.Total
	}
	if len(c.Limits.PerCapability) > 0 {
		out.PerCapability = make(map[string]int, len(c.Limits.PerCapability))
		for name, v := range c.Limits.PerCapability {
			out.PerCapability[name] = v
		}
	}
	return out
}

// SortedPassthrough 返回排好序的白名单拷贝（仅用于本机进程内诊断，不上报）。
func (c *Config) SortedPassthrough() []string {
	out := append([]string(nil), c.EnvPassthrough...)
	sort.Strings(out)
	return out
}
