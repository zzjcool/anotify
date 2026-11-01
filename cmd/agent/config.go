package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/anotify/internal/ws"
	"gopkg.in/yaml.v3"
)

// 本文件是 node.yaml 的**配置层**：解码、校验，以及把配置翻译成 hello 帧。
//
// node.yaml 只剩 bootstrap 四件（契约 §1 / 裁决 D4）：
//
//	name: buildbox-1
//	server: https://anotify.example.com
//	token: nt_xxxxx
//	instances: 4          # 并发槽位数（可选，缺省 1）
//
// **能力不在机器上声明了**。能力由控制台唯一定义、编译成产物（internal/artifact），
// 经 deploy 帧下发到本机缓存（见 cache.go / apply.go）。节点启动、重连时只是如实
// 上报「我缓存了哪些产物」（hello.applied），而「这个能力归谁」的唯一写者是中枢的
// 部署表。
//
// 这条边界就是 D4：node_capabilities 从「节点自报的能力事实」降级为「已部署产物
// 的派生索引」。节点不能再单方面声称「我会跑 X」——否则中枢数据库会静默撒谎
// （页面显示能跑，真派下来却 failed）。

// nodeConfig 是 node.yaml 的全部内容（节点侧唯一的 bootstrap 声明入口）。
//
// 节点与中枢解耦、部署在用户侧，所以「连哪、用哪个令牌、开几个槽位」必须由本地
// 配置声明；而「跑什么」不在这里——那是中枢部署下来的产物（见上面的注释）。
type nodeConfig struct {
	Name   string `yaml:"name"`
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
	// Instances 是并发槽位数。<=0 归一为 1（见 slots），
	// 它是节点**自己**的资源声明，中枢只能少用、不能多用（契约 D5）。
	Instances int `yaml:"instances"`
}

// loadConfig 读取并校验一份 node.yaml。
//
// 所有错误都以**文件路径**为前缀（用户同时维护多份 node.yaml 时，报错必须自己
// 说清是哪一份）。未知字段是错误：拼错的字段名静默失效比启动失败更难查——
// 这条同时也是一道迁移护栏：v1 的 `capabilities:` 现在就是未知字段，老配置会在
// 启动时被明确拒绝，而不是静默连上去却永远接不到单。
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
	problems := make([]string, 0, 4)

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

// hello 把 bootstrap 配置 + 本机缓存索引翻译成 hello 帧（契约 §3）。
//
// applied 是**从哪里来**的关键：它不是「我能跑什么」，而是「我缓存了什么」。
// 中枢拿到它只做一件事——和 deployments 对账（对上了 → applied；digest 不一致
// → local 本地变体），绝不会因为出现一条 applied 就把能力归属给这台机器。
//
// 参数由调用方从 cache 取（`c.cache.applied()`），而不是在这里构造 cache：
// 这样 config 层保持纯函数、可离线单测（配置解析与缓存落盘是两件事）。
func (c *nodeConfig) hello(applied []appliedDecl) *helloFrame {
	slots := c.slots()
	perSlot := c.slotConcurrency()
	instances := make([]instanceDecl, 0, slots)
	for slot := 0; slot < slots; slot++ {
		instances = append(instances, instanceDecl{Slot: slot, Concurrency: perSlot})
	}
	// 空列表纪律：hello.applied 必须是 []，不是 null（服务端/前端把 null 当异常）。
	if applied == nil {
		applied = []appliedDecl{}
	}

	return &helloFrame{
		Type:      ws.NodeFrameHello,
		Protocol:  ws.ProtocolVersion,
		NodeID:    "",
		Instances: instances,
		Applied:   applied,
	}
}

// slots 是并发槽位数：instances 缺省（<=0）归一为 1。
//
// 归一在**读取处**表达，不改写用户结构体：配置文件里没写就是没写，
// 结构体的零值仍然忠实反映「用户没声明」，只有使用它的地方才按缺省语义算。
func (c *nodeConfig) slots() int {
	if c.Instances <= 0 {
		return 1
	}
	return c.Instances
}

// slotConcurrency 是单个槽位里能并跑几个任务。
//
// v1 这个值来自 node.yaml 里各能力的 run.concurrency（节点自报执行方式的一部分）；
// v2 执行方式归产物、节点不再声明它，所以一个槽位就只放一个任务——**并发上限
// 完全由节点启动参数（instances）决定**，中枢推不动（契约 D5）。
func (c *nodeConfig) slotConcurrency() int {
	return 1
}

// taskLimit 是单个能力的并发上限 = 槽位数 × 每槽并发 = slots()。
//
// 用「每能力一个信号量」而不是一个全局信号量：一个慢能力不该把别的能力堵死；
// 而节点整体的并发上限靠 instances（槽位数）在启动参数里收口。
func (c *nodeConfig) taskLimit() int {
	return c.slots() * c.slotConcurrency()
}

// defaultDataDir 是节点数据目录的缺省值：~/.anotify。
//
// 布局（契约 §5 + T4 任务卡）：
//
//	<data-dir>/artifacts/<digest>/capability.yaml   产物原文
//	<data-dir>/artifacts/<digest>/files/<rel-path> 展开的文件
//	<data-dir>/index/<name>@<version>              单行文本：digest
//	<data-dir>/work/<task_id>/                     每次任务独立工作目录
//
// 用用户主目录而不是 /tmp：产物是**要留着的**（断电重启后靠 hello.applied 对账），
// 放 /tmp 会被系统清理，节点重启就变成「什么都不会跑」。
func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for the default data dir: %w", err)
	}
	return filepath.Join(home, ".anotify"), nil
}
