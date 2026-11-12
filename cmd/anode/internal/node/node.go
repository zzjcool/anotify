// Package node 把 N2A 的配置/缓存/权限闭包装配成一个可测试的节点对象。
//
// 它不启动 WS 连接、不监听 emit、不执行任务：这些分别属于 hub.Client（配置帧
// 流程）和 N2B 的 runtime/emit 网关。本包只回答四件事：
//
//   - hello 快照（身份 + 本机配置 + applied 版本 + 内置事实 + 缓存能力探测 + 槽位）；
//   - welcome 应用（local > node 合并 → protocol.MachineConfigHash → 原子落盘 →
//     才产生 config.applied）；
//   - 生效命名空间 = 本机申报 ∩ 中枢授权（本地不能扩张）；
//   - 显式 capability.deploy → 拉取/校验/缓存 → 同 refs 的 capability.deployed。
package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/zzjcool/anotify/cmd/anode/internal/applied"
	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Options 是 Node 的装配参数（全部显式，避免包内读全局/环境）。
type Options struct {
	// Config 是已校验的 anode.yaml。
	Config *config.Config
	// Store 是 applied 文件读写入口（通常 applied.NewStore(dataDir)）。
	Store *applied.Store
	// Cache 是产物缓存（通常 capability.NewCache(filepath.Join(dataDir, "capabilities"))）。
	Cache *capability.Cache
	// Baseline 是守护进程启动环境的快照（PATH 与命令探测同源）。
	Baseline hostenv.Baseline
	// Facts 是节点内置事实（anode.md §3.6）；零值时用 RuntimeFacts 补。
	Facts protocol.NodeFacts
	// AgentVersion / AgentCommit 是自报版本事实（空串 = 未知）。
	AgentVersion string
	AgentCommit  string
	// Fetcher 是产物拉取客户端；nil 时用 Config.Server / Config.Token 构造。
	Fetcher *capability.Fetcher
}

// Node 是单台节点的配置/缓存事实源。
type Node struct {
	cfg      *config.Config
	store    *applied.Store
	cache    *capability.Cache
	baseline hostenv.Baseline
	facts    protocol.NodeFacts
	version  string
	commit   string
	fetcher  *capability.Fetcher
	deployer *capability.Deployer

	mu        sync.Mutex
	applied   protocol.AppliedConfig
	present   bool
	effective []string
	loadErr   error
	// instanceSource 是真实物理槽位账本（A3；nil = 还没接线，只报空槽）。
	instanceSource func() []protocol.HelloInstance
}

// New 装配节点：加载已有 applied（缺失 = 冷启动；损坏 = 不信任并等待中枢对齐）。
func New(opts Options) (*Node, error) {
	if opts.Config == nil {
		return nil, errors.New("node: 缺少配置")
	}
	if opts.Store == nil {
		return nil, errors.New("node: 缺少 applied store")
	}
	if opts.Cache == nil {
		return nil, errors.New("node: 缺少产物缓存")
	}
	n := &Node{
		cfg:      opts.Config,
		store:    opts.Store,
		cache:    opts.Cache,
		baseline: opts.Baseline,
		facts:    opts.Facts,
		version:  opts.AgentVersion,
		commit:   opts.AgentCommit,
		// welcome 之前只知道申报值；授权由 welcome 收窄（本地不能扩张）。
		effective: opts.Config.DeclaredNamespaces(),
	}
	if n.facts == (protocol.NodeFacts{}) {
		n.facts = RuntimeFacts("", opts.Baseline.HOME())
	}
	loaded, present, err := opts.Store.Load()
	if err != nil {
		// 手改/损坏的 applied 就是 drift：不把它当生效事实，hello 报「没有 applied」，
		// 等 welcome 重新合并落盘（config.md §4.3）。
		n.loadErr = err
	} else if present {
		if verr := config.ValidateNodeDefaults(loaded.Vars, loaded.Limits); verr != nil {
			n.loadErr = fmt.Errorf("applied 内容不合法: %w", verr)
		} else {
			n.applied = loaded
			n.present = true
		}
	}
	if opts.Fetcher != nil {
		n.fetcher = opts.Fetcher
	} else {
		fetcher, ferr := capability.NewFetcher(opts.Config.Server, opts.Config.Token)
		if ferr != nil {
			return nil, ferr
		}
		n.fetcher = fetcher
	}
	n.fetcher.AllowNamespace = func(namespace string) bool { return n.AllowsNamespace(namespace) }
	n.deployer = &capability.Deployer{Fetcher: n.fetcher, Cache: n.cache, LookPath: n.baseline.LookPath}
	return n, nil
}

// Config 返回本机配置（只读；字段在校验后不再变）。
func (n *Node) Config() *config.Config { return n.cfg }

// Name 返回节点名。
func (n *Node) Name() string { return n.cfg.Name }

// AppliedError 返回启动时 applied 文件的加载错误（nil = 正常/冷启动）。
func (n *Node) AppliedError() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.loadErr
}

// AppliedPresent 报告当前是否有一份可信的 applied。
func (n *Node) AppliedPresent() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.present
}

// AppliedState 返回 hello/config.get 里的 (appliedRevision, appliedHash)。
// 冷启动或 applied 不可信时两者都是空串——不伪造「上次在线」。
func (n *Node) AppliedState() (string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.present {
		return "", ""
	}
	return n.applied.Revision, n.applied.Hash
}

// EffectiveNamespaces 返回申报 ∩ 授权（排序拷贝）。
func (n *Node) EffectiveNamespaces() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.effective...)
}

// AllowsNamespace 报告一个 ns 是否被本节点授权（部署/派发前的本地预检）。
func (n *Node) AllowsNamespace(namespace string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return config.AllowsNamespace(n.effective, namespace)
}

// Hello 组装连接中枢的第一帧（每次连接/重连都重新取当前事实）。
func (n *Node) Hello() protocol.HelloFrame {
	n.mu.Lock()
	defer n.mu.Unlock()
	rev, hash := "", ""
	if n.present {
		rev, hash = n.applied.Revision, n.applied.Hash
	}
	localVars := make(map[string]string, len(n.cfg.Vars))
	for k, v := range n.cfg.Vars {
		localVars[k] = v
	}
	total := 0
	if n.present {
		total = n.applied.Limits.Total
	}
	frame := protocol.HelloFrame{
		Type:            protocol.FrameHello,
		Name:            n.cfg.Name,
		Token:           n.cfg.Token,
		Namespaces:      append([]string(nil), n.cfg.Namespaces...),
		LocalVars:       localVars,
		LocalLimits:     n.cfg.LocalLimits(),
		AppliedRevision: rev,
		AppliedHash:     hash,
		AgentVersion:    n.version,
		AgentCommit:     n.commit,
		Facts:           n.facts,
		Capabilities:    n.capabilitiesLocked(),
	}
	n.mu.Unlock()
	// 槽位账本可能反查机器级生效值（runner → Applied → MachineState）：必须在
	// 释放 n.mu 之后取，否则自锁。
	frame.Instances = n.instances(total)
	n.mu.Lock() // 与 defer Unlock 配对（调用方仍持锁到返回）
	return frame
}

// ApplyWelcome 应用中枢应答（hello 的 welcome 或 config.get 的应答）。
//
// 返回 nil 帧 = 幂等（同一 revision + hash，且本地值没变）：不重复写盘、不重复
// 回报；返回非 nil = 已成功落盘，调用方应发送 config.applied。
// Save 失败时状态不变、也不返回帧（central 仍看到旧 applied，下一轮再对齐）。
func (n *Node) ApplyWelcome(welcome protocol.WelcomeFrame) (*protocol.ConfigAppliedFrame, error) {
	if welcome.Type != protocol.FrameWelcome {
		return nil, fmt.Errorf("node: 期望 welcome，实际 %q", welcome.Type)
	}
	if err := config.ValidateNodeDefaults(welcome.NodeVars, welcome.NodeLimits); err != nil {
		return nil, err
	}
	vars, limits, hash, err := config.HashMachine(n.cfg, welcome.NodeVars, welcome.NodeLimits)
	if err != nil {
		return nil, err
	}
	effective := config.EffectiveNamespaces(n.cfg.Namespaces, welcome.Namespaces)

	n.mu.Lock()
	// 授权收窄/放开立即生效，不依赖配置是否变化。
	n.effective = effective
	if n.present && n.applied.Revision == welcome.ConfigRevision && n.applied.Hash == hash {
		n.mu.Unlock()
		return nil, nil
	}
	stored, err := n.store.Save(welcome.ConfigRevision, vars, limits)
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	n.applied = stored
	n.present = true
	n.loadErr = nil
	total := stored.Limits.Total
	n.mu.Unlock()
	// A3：同类型 instances 快照（预算/槽位变化后中枢能对齐）；取账本不持锁。
	return &protocol.ConfigAppliedFrame{
		Type:        protocol.FrameConfigApplied,
		Revision:    stored.Revision,
		Hash:        stored.Hash,
		MachineVars: cloneVars(stored.Vars),
		Instances:   n.instances(total),
	}, nil
}

// HasCapability 报告某个 digest 是否已安全缓存（存在且通过完整校验）。
func (n *Node) HasCapability(digest string) bool { return n.cache.Has(digest) }

// MachineState 返回机器级生效配置快照（revision + local>node 合并后的 vars/limits）。
//
// present=false = 冷启动或 applied 不可信：调用方不能把它当作「已 applied」。
func (n *Node) MachineState() (revision string, vars map[string]string, limits protocol.Limits, present bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.present {
		return "", map[string]string{}, protocol.Limits{}, false
	}
	return n.applied.Revision, cloneVars(n.applied.Vars), n.applied.Limits, true
}

// LoadArtifact 按 digest 读取缓存产物（digest/内容/spec 全验）；派发接受时用。
func (n *Node) LoadArtifact(digest string) (*artifact.Artifact, error) {
	entry, err := n.cache.Load(digest)
	if err != nil {
		return nil, err
	}
	return entry.Artifact, nil
}

// HandleDeploy 处理一条显式 capability.deploy：真实 GET、校验、缓存、回执。
func (n *Node) HandleDeploy(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame {
	return n.deployer.Handle(ctx, frame)
}

// capabilitiesLocked 用缓存里的产物回答 hello.capabilities（命令探测用启动 PATH）。
//
// 探测与执行同源：这里只调用 n.baseline.LookPath（启动快照），不读 os.Getenv。
func (n *Node) capabilitiesLocked() []protocol.HelloCapability {
	entries, _ := n.cache.Entries()
	out := make([]protocol.HelloCapability, 0, len(entries))
	for _, entry := range entries {
		artifact := entry.Artifact
		commands := make([]protocol.HelloCommand, 0, len(artifact.Spec.Requires.Commands))
		for _, name := range artifact.Spec.Requires.Commands {
			cmd := protocol.HelloCommand{Name: name}
			if path, err := n.baseline.LookPath(name); err == nil {
				cmd.Found = true
				cmd.Path = path
			}
			commands = append(commands, cmd)
		}
		out = append(out, protocol.HelloCapability{
			Name:     entry.Name,
			Version:  entry.Version,
			Digest:   entry.Digest,
			Commands: commands,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Digest < out[j].Digest
	})
	return out
}

// instances 申报本机物理槽位（hello.instances / config.applied.instances）。
//
// A3：槽位事实来自运行器的真实账本（含缩预算后超范围的存量占用）；没有账本源
// 时退回「按生效 limits 报空槽」，冷启动没有 applied 时为空列表（不造上次的槽位）。
//
// **调用时不能持有 n.mu**：账本可能反查 Applied → MachineState（自锁）。
func (n *Node) instances(total int) []protocol.HelloInstance {
	n.mu.Lock()
	src := n.instanceSource
	n.mu.Unlock()
	if src != nil {
		return src()
	}
	if total < 0 {
		total = 0
	}
	out := make([]protocol.HelloInstance, 0, total)
	for slot := 0; slot < total; slot++ {
		idx := slot
		no := false
		out = append(out, protocol.HelloInstance{Slot: &idx, Busy: &no})
	}
	return out
}

// SetInstanceSource 绑定真实槽位账本（运行器实现）；配置应用后也要能上报。
func (n *Node) SetInstanceSource(src func() []protocol.HelloInstance) {
	n.mu.Lock()
	n.instanceSource = src
	n.mu.Unlock()
}

// RuntimeFacts 采集节点内置事实（anode.md §3.6）。
//
// workRoot 为空时留空（由调用方在 B 阶段传任务根目录）；homeDir 为空时取
// 当前用户家目录。IP 取第一个非 loopback IPv4，没有就 127.0.0.1。
func RuntimeFacts(workRoot, homeDir string) protocol.NodeFacts {
	hostname, _ := os.Hostname()
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return protocol.NodeFacts{
		HostIP:   primaryIPv4(),
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPU:      runtime.NumCPU(),
		WorkRoot: workRoot,
		HomeDir:  homeDir,
	}
}

func primaryIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String()
		}
	}
	return "127.0.0.1"
}

func cloneVars(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return out
}
