package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/zzjcool/anotify/internal/ws"
	"gopkg.in/yaml.v3"
)

// 本文件是 node.yaml 的**配置层**：解码、校验，以及把配置翻译成 hello 帧里的
// capabilities / manifest / instances 声明。
//
// 边界（契约 I1）：命令只来自本地 node.yaml 的 run.command —— 中枢不下发命令、
// 也不解释执行方式。本文件**不拼接命令、不做 {{param}} 插值**：run.command 原样
// 交给 runner 的 `sh -c`，参数只走环境变量（见 runner.go）。
//
// 帧类型不在这里定义：helloFrame / capabilityDecl / manifestDecl / instanceDecl
// 是 protocol.go（W2）复用的 internal/ws 类型别名。这里只从 internal/ws 取两个
// **常量**（帧类型名与协议版本），不碰帧结构。

// nodeConfig 是 node.yaml 的全部内容（节点侧唯一声明入口）。
//
// 节点与中枢解耦、部署在用户侧，所以它的能力必须由这份本地配置声明
// （docs/design/playbooks.md §4）。
type nodeConfig struct {
	Name         string             `yaml:"name"`
	Server       string             `yaml:"server"`
	Token        string             `yaml:"token"`
	Instances    int                `yaml:"instances"`
	Capabilities []capabilityConfig `yaml:"capabilities"`
}

// capabilityConfig 是一条能力声明：能力名 + 版本 + 它**怎么跑**。
type capabilityConfig struct {
	Capability string   `yaml:"capability"`
	Version    int      `yaml:"version"`
	Run        *runSpec `yaml:"run"`
}

// runSpec 是 node.yaml 的 run 块（#168 的形状），同时也是 manifest 里的 run 原文。
//
// 字段是节点自己的执行方式声明，中枢只校验形状、只存不执行（I1）。yaml 与 json
// 双 tag：yaml 读本地配置，json 用于 manifest 里的 run 原文（线上帧字段名以
// internal/ws 的 json tag 为准，这里是数组内嵌的 raw JSON，所以自己带 tag）。
type runSpec struct {
	Runner      string            `yaml:"runner"      json:"runner"`
	Command     string            `yaml:"command"     json:"command"`
	TimeoutSec  int               `yaml:"timeoutSec"  json:"timeoutSec,omitempty"`
	Concurrency int               `yaml:"concurrency" json:"concurrency,omitempty"`
	Env         map[string]string `yaml:"env"         json:"env,omitempty"`
	// Out 是**成功时**回报的出口名（喂给中枢的多跳选边，见 playbooks §4）。
	// 缺省 = 不回报（result 帧里 out 键缺席 → 分支终止）。
	// 节点侧不做插值/映射/校验：它就是一段固定字符串，合法性由服务端查边时判定。
	Out string `yaml:"out" json:"out,omitempty"`
}

// loadConfig 读取并校验一份 node.yaml。
//
// 所有错误都以**文件路径**为前缀（用户同时维护多份 node.yaml 时，报错必须自己
// 说清是哪一份）。未知字段是错误：拼错的字段名静默失效比启动失败更难查。
func loadConfig(path string) (*nodeConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // 未知字段 = 错误（防拼错静默失效）
	cfg := &nodeConfig{}
	// 空文件（io.EOF）不算解码错误：交给 validate 把「缺什么」一次列全。
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		// yaml 库自己的报错已经带行号与字段名，这里只补文件路径前缀。
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validate 汇总式校验：**一次列全**所有问题（用 "; " 连接），不是只报第一条。
//
// node.yaml 是手写的，逐条报错会变成「改一行跑一次」的猜谜。
func (c *nodeConfig) validate() error {
	problems := make([]string, 0, 8)

	// server：必须能解析出 host、且能映射到 ws/wss（不让用户猜 scheme）。
	if _, err := c.streamURL(); err != nil {
		problems = append(problems, err.Error())
	}
	if strings.TrimSpace(c.Token) == "" {
		problems = append(problems, "token is required")
	}
	if c.Instances < 0 {
		problems = append(problems, "instances must be >= 0")
	}
	if len(c.Capabilities) == 0 {
		problems = append(problems, "at least one capability is required")
	}

	seen := make(map[string]struct{}, len(c.Capabilities))
	for i, cc := range c.Capabilities {
		ref := cc.Capability
		if strings.TrimSpace(ref) == "" {
			// 名字都没有时 {ref} 渲染不出来，用 #<index> 自描述
			// （与中枢侧 confirmCapabilityDecls 的口径一致）。
			problems = append(problems, fmt.Sprintf("#%d: capability is required", i))
			ref = fmt.Sprintf("#%d", i)
		}
		if cc.Version < 1 {
			problems = append(problems, fmt.Sprintf("%s: version must be >= 1", ref))
		}
		full := fmt.Sprintf("%s@%d", ref, cc.Version)
		if _, dup := seen[full]; dup {
			problems = append(problems, "duplicate capability "+full)
		}
		seen[full] = struct{}{}

		if cc.Run == nil {
			problems = append(problems, full+": run is required")
			continue
		}
		switch cc.Run.Runner {
		case "":
			problems = append(problems, full+": run.runner is required (only bash)")
		case "bash":
			// 本期唯一执行器（playbooks §4.1：必须显式写，不隐式默认成 bash）。
		default:
			problems = append(problems,
				fmt.Sprintf("%s: run.runner %q is not supported (only bash)", full, cc.Run.Runner))
		}
		if strings.TrimSpace(cc.Run.Command) == "" {
			problems = append(problems, full+": run.command is empty")
		}
		// 0 = 缺省（归一为 1，见 slots/slotConcurrency/taskLimit）；负数才是错误。
		if cc.Run.Concurrency < 0 {
			problems = append(problems, full+": run.concurrency must be >= 0")
		}
		if cc.Run.TimeoutSec < 0 {
			problems = append(problems, full+": run.timeoutSec must be >= 0")
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// streamURL 把 server 映射到节点流端点（GET /v1/node/stream）。
//
// 规则（契约 §3.1.2）：http→ws、https→wss，ws/wss 原样保留；追加固定路径
// （已经带了就不重复追加）；**无 scheme 一律报错**——不猜用户想连哪儿。
func (c *nodeConfig) streamURL() (string, error) {
	const streamPath = "/v1/node/stream"

	raw := strings.TrimSpace(c.Server)
	if raw == "" {
		return "", errors.New("server is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("server %q is not a valid URL: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		// 含 "localhost:8080" 这种：url.Parse 会把 localhost 当 scheme，
		// 于是没有 host —— 这里一并挡掉。
		return "", fmt.Errorf("server %q is not a valid URL (scheme must be http, https, ws or wss)", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server %q is not a valid URL (missing host)", raw)
	}
	path := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(path, streamPath) {
		path += streamPath
	}
	u.Path = path
	return u.String(), nil
}

// hello 把配置翻译成 hello 帧的声明部分。
//
// 顺序即 yaml 顺序（同一个 node.yaml 每次启动发出的帧都一致，便于排障）。
// node_id 留空：由服务端按令牌认（node.yaml 不提供 node_id）。
// manifest[].run 是 runSpec 的**紧凑 JSON 原文**：中枢只存不执行，所以这里发给
// 服务端的形状与本地配置一一对应（zero 值字段由 omitempty 省略）。
func (c *nodeConfig) hello() *helloFrame {
	caps := make([]capabilityDecl, 0, len(c.Capabilities))
	manifest := make([]manifestDecl, 0, len(c.Capabilities))
	for _, cc := range c.Capabilities {
		caps = append(caps, capabilityDecl{Capability: cc.Capability, Version: cc.Version})

		var run json.RawMessage
		if cc.Run != nil {
			// runSpec 只有标量/map[string]string，json.Marshal 不会失败；万一失败
			// 就留 nil（服务端会以 run is required 拒绝），好过发出半截 run。
			if b, err := json.Marshal(*cc.Run); err == nil {
				run = b
			}
		}
		manifest = append(manifest, manifestDecl{
			Capability: cc.Capability,
			Version:    cc.Version,
			Run:        run,
		})
	}

	slots := c.slots()
	perSlot := c.slotConcurrency()
	instances := make([]instanceDecl, 0, slots)
	for slot := 0; slot < slots; slot++ {
		instances = append(instances, instanceDecl{Slot: slot, Concurrency: perSlot})
	}

	return &helloFrame{
		Type:         ws.NodeFrameHello,
		Protocol:     ws.ProtocolVersion,
		NodeID:       "",
		Capabilities: caps,
		Manifest:     manifest,
		Instances:    instances,
		Labels:       nil,
	}
}

// lookup 按 name@version 找本节点声明的 run 块。
//
// 第二个返回值 false 表示「没有可用的 run」（既包括没声明这个能力，也包括声明了
// 但没写 run）：两种情况节点都无法执行，client 侧一律 ack 后回 result failed。
func (c *nodeConfig) lookup(capability string, version int) (*runSpec, bool) {
	for i := range c.Capabilities {
		cc := &c.Capabilities[i]
		if cc.Capability == capability && cc.Version == version {
			return cc.Run, cc.Run != nil
		}
	}
	return nil, false
}

// slots 是并发槽位数：instances 缺省（<=0）归一为 1。
func (c *nodeConfig) slots() int {
	if c.Instances <= 0 {
		return 1
	}
	return c.Instances
}

// slotConcurrency 是每个槽位里能并跑几个任务：全部 run.concurrency 的 max，
// 缺省（<=0）记 1。
func (c *nodeConfig) slotConcurrency() int {
	best := 0
	for _, cc := range c.Capabilities {
		if cc.Run == nil {
			continue
		}
		n := cc.Run.Concurrency
		if n <= 0 {
			n = 1
		}
		if n > best {
			best = n
		}
	}
	if best <= 0 {
		return 1
	}
	return best
}

// taskLimit 是某个能力的并发上限：槽位数 × 该能力声明的 concurrency。
//
// rs == nil（沿用不到 run 块）时退化成一个槽位一个任务 = slots()。
func (c *nodeConfig) taskLimit(rs *runSpec) int {
	if rs == nil {
		return c.slots()
	}
	n := rs.Concurrency
	if n <= 0 {
		n = 1
	}
	return c.slots() * n
}
