package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// WorkDirs 是**本机**对「能力工作目录」的授权（迁移 19 → 能力级）。
	//
	// 为什么必须在节点侧：控制台可以为某个能力指定「在这个目录里跑」，而那条路径
	// 来自中枢。如果直接 chdir 过去，等于「谁改了中枢，谁就能让本机以 agent 身份
	// 在任意目录读写」。白名单把这条权力收回到机器主人手里：中枢只能从你划定的
	// 范围里选，选到范围外就失败（并回报原因），不会静默退回默认目录。
	//
	// 两种写法都接受（见 WorkDirMap）：能力级映射，或旧的扁平目录列表。
	// 空 = 一个都不许（中枢指定 workDir 的能力在这台机器上必然失败）。
	WorkDirs WorkDirMap `yaml:"workDirs"`
}

// workDirsAnyKey 是「兜底白名单」在 workDirs 里的键。
//
// 它不是能力名（能力名来自产物 metadata.name，形如 script.echo），所以和真实能力
// 不会撞；用它当键是为了让新写法能表达旧语义（任意能力都可用的前缀白名单），
// 而不必在 yaml 里发明第二个顶层字段。
const workDirsAnyKey = "*"

// WorkDirMap 是 `workDirs` 的**各种写法归一后**的结果。
//
// 为什么需要自定义解码：yaml 里同一个键的形态有四种，而且**都已存在**——
// install-agent.sh 生成的配置就落在其中三种上：
//
//	workDirs:                    # ① 能力级：值是单个目录
//	  script.echo: /home/me/proj
//	  build.repo: /srv/app
//	  "*": /srv/projects        # ② 兜底：值是单个目录
//
//	workDirs:
//	  script.echo: /home/me/proj
//	  "*":                       # ③ 兜底：值是**目录列表**（--work-dirs=a:b 生成这个）
//	    - /srv/shared
//	    - /srv/other
//
//	workDirs:                    # ④ 旧写法：整份就是目录列表（等价于全部塞进 "*"）
//	  - /home/me/project
//	  - /srv/apps
//
// 归一是刻意的：解析顺序只有一处实现（runner.resolveChdir），而「旧写法」只是
// 「兜底白名单非空」这一种情况，不需要第二套分支去维护。
type WorkDirMap struct {
	// byCapability 是**能力级**的目录：命中时压过中枢 assign 帧里的 work_dir。
	// 它本身就是机器主人的授权，因此不再受 fallback 的前缀限制。
	//
	// 用 []string 而不是 string：yaml 里每个 value 既可能是字符串也可能是字符串
	// 列表（见上面的 ①③），解析层必须都能吃下。至于「一个能力只跑在一个目录里」，
	// 交给 validate 收紧（写两个会被明确拒绝，而不是悄悄只用其中一个）。
	//
	// 键**原样**使用，不做大小写/下划线规整：install-agent.sh --work-dir=<能力>:<路径>
	// 生成的 key 就是产物 metadata.name（[a-z0-9._-]）。任何规整都会让「yaml 里写了
	// script.echo，脚本却跑在别处」变成一个查不出来的谜。
	byCapability map[string][]string
	// fallback 是 `"*"` 下的兜底前缀白名单（旧语义）：中枢给的 work_dir 必须落在其中。
	fallback []string
}

// UnmarshalYAML 让 `workDirs` 同时接受映射与列表两种写法（yaml.v3 的钩子）。
//
// 注意它只做**形状**归一，不做路径合法性校验：校验集中在 validate（一次列全），
// 解码阶段就报错会让「同一份 yaml 有 3 处写错」变成改一处跑一次的猜谜。
func (m *WorkDirMap) UnmarshalYAML(node *yaml.Node) error {
	parsed := WorkDirMap{}
	if err := parseWorkDirs(node, &parsed); err != nil {
		return err
	}
	*m = workDirsFrom(parsed.byCapability, parsed.fallback)
	return nil
}

// For 返回本机为某个能力**指定**的目录（workDirs[<能力名>]）。
//
// 第二个返回值是「本机给这个能力写了目录」——调用方据此决定是否忽略中枢的
// work_dir（机器事实压过中枢意图）。注意 `"*"` 不是能力，永远不从这里返回：
// 它是兜底白名单，语义完全不同（一个是指定目录，一个是允许前缀），由 Allowlist 给。
func (m WorkDirMap) For(capability string) (string, bool) {
	name := strings.TrimSpace(capability)
	if name == "" || name == workDirsAnyKey {
		return "", false
	}
	for _, dir := range m.byCapability[name] {
		if d := strings.TrimSpace(dir); d != "" {
			return d, true
		}
	}
	// 空值/空列表只可能出现在校验失败的配置里（validate 会拦），这里再收一次口：
	// 宁可退回旧语义，也不要拿一个不存在的目录去 chdir。
	return "", false
}

// Allowlist 是 `"*"` 下的兜底前缀白名单（旧写法 / ANOTIFY_WORK_DIRS 等价物）。
//
// 返回副本：这份切片会被 runner 一路带到错误信息里，不该让任何调用方原地改到配置。
func (m WorkDirMap) Allowlist() []string {
	if len(m.fallback) == 0 {
		return nil
	}
	return append([]string(nil), m.fallback...)
}

// workDirsFrom 直接造一份 WorkDirMap（环境变量路径与单测用）。
//
// 空容器归一成 nil：零值必须严格等于「什么都没配」，否则 reflect.DeepEqual 之类的
// 结构体断言（config_test）会被「空而非 nil 的 map」判成不相等。
func workDirsFrom(byCapability map[string][]string, fallback []string) WorkDirMap {
	m := WorkDirMap{byCapability: byCapability, fallback: fallback}
	if len(m.byCapability) == 0 {
		m.byCapability = nil
	}
	if len(m.fallback) == 0 {
		m.fallback = nil
	}
	return m
}

// problems 把 workDirs 的问题追加进 validate 的汇总列表。
//
// 报错顺序按能力名排序：yaml 映射是无序的，不排序的话同一份错配置每次跑出来的
// 错误顺序都可能不同，用户会以为改坏了别的东西。每条都要点名「哪个能力/哪一项」，
// 否则用户改的是 5 行 workDirs 里的哪一行全靠猜。
func (m WorkDirMap) problems(problems []string) []string {
	names := make([]string, 0, len(m.byCapability))
	for name := range m.byCapability {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dirs := m.byCapability[name]
		switch {
		case strings.TrimSpace(name) == "":
			problems = append(problems, "workDirs 的能力名不能为空")
			continue
		case len(dirs) > 1:
			problems = append(problems, fmt.Sprintf(
				`workDirs[%q] 只能给一个目录（一个能力只跑在一个目录里）；兜底多个前缀请写 %q: [...]`,
				name, workDirsAnyKey))
			continue
		}
		if len(dirs) == 0 {
			problems = append(problems, fmt.Sprintf("workDirs[%q] 缺少目录", name))
			continue
		}
		dir := strings.TrimSpace(dirs[0])
		if dir == "" {
			problems = append(problems, fmt.Sprintf("workDirs[%q] 的目录不能为空", name))
			continue
		}
		if !filepath.IsAbs(dir) {
			problems = append(problems, fmt.Sprintf("workDirs[%q] 必须是绝对路径：%s", name, dir))
		}
	}
	for i, dir := range m.fallback {
		if !filepath.IsAbs(dir) {
			problems = append(problems, fmt.Sprintf(
				`workDirs 第 %d 项必须是绝对路径：%q（列表写法等价于 %q 下的白名单）`,
				i+1, dir, workDirsAnyKey))
		}
	}
	return problems
}

// parseWorkDirs 把 yaml 节点归一成 WorkDirMap（只认形状，不判路径合法）。
func parseWorkDirs(node *yaml.Node, out *WorkDirMap) error {
	node = derefYAMLNode(node)
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil
		}
		return parseWorkDirs(node.Content[0], out)
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil // `workDirs:` 后面什么都没写 = 空
		}
		return fmt.Errorf("workDirs 必须是「能力名 → 目录」的映射或目录列表，收到单值 %q", node.Value)
	case yaml.MappingNode:
		return parseWorkDirsMapping(node, out)
	case yaml.SequenceNode:
		for i, item := range node.Content {
			item = derefYAMLNode(item)
			if item.Kind == yaml.MappingNode {
				// 混写：列表里出现 `- 能力名: 目录`（能力级条目混在旧列表里）。
				if err := parseWorkDirsMapping(item, out); err != nil {
					return err
				}
				continue
			}
			dir, ok, err := yamlScalarText(item)
			if err != nil {
				return fmt.Errorf("workDirs 第 %d 项不是目录字符串：%w", i+1, err)
			}
			if !ok {
				return fmt.Errorf("workDirs 第 %d 项为空", i+1)
			}
			out.fallback = append(out.fallback, dir)
		}
		return nil
	}
	return errors.New("workDirs 必须是「能力名 → 目录」的映射或目录列表")
}

// parseWorkDirsMapping 解析映射形态：键要么是能力名，要么是 `"*"`；
// 值既可能是单个目录，也可能是目录列表（见 WorkDirMap 的 ①②③）。
func parseWorkDirsMapping(node *yaml.Node, out *WorkDirMap) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := derefYAMLNode(node.Content[i])
		valNode := derefYAMLNode(node.Content[i+1])

		// 键必须是标量。null 键解成空串，交给 validate 报「能力名不能为空」——
		// 在这里报会让 yaml 里的其它问题（相对路径、别的空键）得改一次跑一次。
		if keyNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("workDirs 的能力名必须是字符串（第 %d 个键不是）", i/2+1)
		}
		key, _, err := yamlScalarText(keyNode)
		if err != nil {
			return fmt.Errorf("workDirs 的能力名必须是字符串：%q", keyNode.Value)
		}
		dirs, err := workDirsValueList(valNode)
		if err != nil {
			return fmt.Errorf("workDirs[%q] %w", key, err)
		}

		if key == workDirsAnyKey {
			out.fallback = append(out.fallback, dirs...)
			continue
		}
		if out.byCapability == nil {
			out.byCapability = make(map[string][]string, len(node.Content)/2)
		}
		if _, dup := out.byCapability[key]; dup {
			// 同一个能力写两遍时，谁赢取决于实现细节——那比报错糟得多。
			return fmt.Errorf("workDirs 里的能力 %q 配置了两次", key)
		}
		out.byCapability[key] = dirs
	}
	return nil
}

// workDirsValueList 把一个值节点读成目录列表：单值（可能为空）或列表。
func workDirsValueList(node *yaml.Node) ([]string, error) {
	node = derefYAMLNode(node)
	if node != nil && node.Kind == yaml.SequenceNode {
		dirs := make([]string, 0, len(node.Content))
		for i, item := range node.Content {
			dir, ok, err := yamlScalarText(derefYAMLNode(item))
			if err != nil || !ok {
				return nil, fmt.Errorf("第 %d 项不是目录字符串", i+1)
			}
			dirs = append(dirs, dir)
		}
		return dirs, nil
	}
	dir, ok, err := yamlScalarText(node)
	if err != nil {
		return nil, fmt.Errorf("不是目录字符串：%w", err)
	}
	if !ok {
		return nil, nil // 空值（`能力名:` 后面留空）：交给 validate 报
	}
	return []string{dir}, nil
}

// yamlScalarText 把标量节点读成字符串（去掉首尾空白）。
//
// 第二个返回值 = 「这里有值」：null（`key:` 后面留空）返回 false，由调用方决定
// 是「空」还是「报错」——同一个空值在 `"*"` 下是合法的，在能力名下不是。
func yamlScalarText(node *yaml.Node) (string, bool, error) {
	if node == nil {
		return "", false, nil
	}
	if node.Kind != yaml.ScalarNode {
		return "", false, errors.New("不是标量")
	}
	if node.Tag == "!!null" {
		return "", false, nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(s), true, nil
}

// derefYAMLNode 跟随 YAML 锚点/别名（`workDirs: *dirs`）。
func derefYAMLNode(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

// 环境变量名（**容器/编排友好**的 bootstrap 入口）。
//
// 为什么要有这一套：控制台的「加机器」面板给的是一行 docker 命令，容器里没有
// node.yaml 可指；而把四件配置挂进镜像等于每个环境打一份镜像。
// 环境变量是那条路径唯一现实的载体。
//
// 优先级：显式 `-f` > 环境变量 > `./node.yaml`（见 loadConfigFrom）。
// 谁都不给时仍然按老路径找 ./node.yaml —— 二进制装在机器上的人不受影响。
const (
	envServer    = "ANOTIFY_SERVER"
	envToken     = "ANOTIFY_TOKEN"
	envName      = "ANOTIFY_NAME"
	envInstances = "ANOTIFY_INSTANCES"
	// ANOTIFY_WORK_DIRS 是 workDirs 的环境变量形态：冒号分隔的绝对路径列表
	// （容器 docker run -e 那一路用它，因为容器里没有 node.yaml）。
	envWorkDirs = "ANOTIFY_WORK_DIRS"
)

// loadConfigFrom 按「显式文件 > 环境变量 > 缺省路径」选一份配置。
//
// explicit 是用户**显式**传的 -f（不是缺省值）：只要他点名了某个文件，就只认
// 那个文件，读不到就报错 —— 绝不悄悄回落到环境变量，否则「我改的文件没生效」
// 会变成一个完全无声的故障。
func loadConfigFrom(explicit string, getenv func(string) string) (*nodeConfig, error) {
	if strings.TrimSpace(explicit) != "" {
		return loadConfig(explicit)
	}
	if cfg, ok := configFromEnv(getenv); ok {
		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("环境变量: %w", err)
		}
		return cfg, nil
	}
	return loadConfig(defaultConfigPath)
}

// configFromEnv 从环境变量拼一份配置。
//
// 判据是 **ANOTIFY_TOKEN 非空**：它是唯一不可推导的一项（server 可以缺省成
// 本机、name 可以由主机名兜底，令牌无从猜测）。没给令牌 = 「没打算走这条路」，
// 返回 false 让调用方回落到 node.yaml。
func configFromEnv(getenv func(string) string) (*nodeConfig, bool) {
	token := strings.TrimSpace(getenv(envToken))
	if token == "" {
		return nil, false
	}
	cfg := &nodeConfig{
		Server: strings.TrimSpace(getenv(envServer)),
		Token:  token,
		Name:   strings.TrimSpace(getenv(envName)),
	}
	if cfg.Server == "" {
		// 容器里最合理的缺省是「回到它自己的宿主机」，但那个地址 agent 猜不到，
		// 而猜错的表现是「启动成功但一直连不上」，极难查。所以留空交给 validate
		// 明确报错，而不是给一个看起来能用、实际连到别处的值。
		cfg.Server = ""
	}
	if raw := strings.TrimSpace(getenv(envInstances)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			// 不在这里报错：让 validate 统一汇报，保持「一次列全」的口径。
			// 用一个一定会失败的标记（负数非法，validate 已有这条），
			// 而不是静默当成 0 —— 那会让 `ANOTIFY_INSTANCES=abc` 变成「1 个槽位」。
			cfg.Instances = -1
			return cfg, true
		}
		cfg.Instances = n
	}
	// 冒号分隔（与 TLS_CERT_FILE 那类列表变量的习惯一致）。空元素丢弃：
	// `A::B` 或结尾多一个冒号都不该变成「允许空路径」。
	//
	// 环境变量形态**只有兜底语义**（等价 workDirs."*"）：环境里塞不下「能力名 →
	// 目录」这种结构化声明，硬编码一套 ANOTIFY_WORK_DIR_<CAP> 只会长出一批没人
	// 记得住的变量名。要能力级目录就写 node.yaml（install-agent.sh --work-dir 走的
	// 就是那条路）。
	var dirs []string
	for _, part := range strings.Split(getenv(envWorkDirs), ":") {
		if d := strings.TrimSpace(part); d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) > 0 {
		cfg.WorkDirs = workDirsFrom(nil, dirs)
	}
	return cfg, true
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
	// workDirs：能力级与兜底都要绝对路径（相对路径的「前缀匹配」会随 cwd 漂移，
	// 那等于白名单形同虚设），能力名不能为空，报错必须点名是哪一条（见 WorkDirMap）。
	problems = c.WorkDirs.problems(problems)

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
