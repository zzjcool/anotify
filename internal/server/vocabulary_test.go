package server

// 用户用词门禁（P7 · capability-spec-v3 §13.4）。
//
// 为什么要有这个测试：仓库里同一个物体长期有三套名字（模型 / 界面 / 代码），
// 界面自己内部还有 2~4 个叫法（「出口」既是 Edge 又是 outputs、「节点」既是
// nodes 表又是流程图里的一张卡）。§13 把用户看到的那一套定死成七个词：
//
//	能力 / 源 / 下游 / 节点 / 部署 / 事件 / 任务
//
// 但「定死」在文案上是**没有编译期约束**的：任何人顺手改一句 locale 就能把
// 旧词放回来，而且 diff 里看不出来。所以这一节要两条机检：
//
//	① 禁词表  —— 旧词短语一个都不许出现在 locale 的**任何**值里；
//	             误报用具名白名单（每条必须写理由），不许整类跳过。
//	② 钉桩表  —— 关键标签的期望值写死。以后想改名，必须**同时**改契约和这里，
//	             「顺手改文案」这条路径被堵死。
//
// 外加一条（同源检查）：四个语言的 key 集合必须完全一致。ja/es 少一个 key
// 时页面会静默回落，而那正是「同一语言里出现两个词」的另一种来源。
//
// 红线：**失败信息要指名道姓**（哪个语言、哪个 key、命中了哪个短语），否则
// 下一个人只会看到一条红，然后去谷歌。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// localeDir 是四份语言表的源目录（sitegen 从 web-src/ 生成 web/i18n.*.js）。
// 测试的工作目录是 internal/server，所以要往上两级。
const localeDir = "../../web-src/locales"

// vocabLangs 是必须一起对齐的语言。顺序只影响失败输出。
var vocabLangs = []string{"zh-CN", "en", "ja", "es"}

// localeStrings 把一份 locale 摊平成 "a.b.c" → 字符串值。
//
// 摊平而不是按结构断言：禁词表要回答「整份文件里还有没有这个词」，
// 逐节写断言会漏掉新加的节 —— 那正是旧词最容易溜回来的地方。
func localeStrings(t *testing.T, lang string) map[string]string {
	t.Helper()
	path := filepath.Join(localeDir, lang+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s：%v（测试的工作目录必须是 internal/server）", path, err)
	}
	var root any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("%s 不是合法 YAML：%v", path, err)
	}
	out := map[string]string{}
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch v := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys) // 失败顺序稳定，diff 才不会乱
			for _, k := range keys {
				child := k
				if prefix != "" {
					child = prefix + "." + k
				}
				walk(child, v[k])
			}
		case []any:
			for i, item := range v {
				walk(fmt.Sprintf("%s[%d]", prefix, i), item)
			}
		case string:
			out[prefix] = v
		case nil:
			out[prefix] = ""
		default:
			out[prefix] = fmt.Sprint(v)
		}
	}
	walk("", root)
	return out
}

// bannedPhrase 是中文侧的一条禁词：短语 + 它为什么被禁（换成什么词）。
type bannedPhrase struct {
	phrase  string
	instead string
}

// zhBannedPhrases 是 §13.1/§13.2 的禁词表。
//
// 逐条对应契约里「从此不许在名词位出现」那一列：
//   - 条边 / 补边 / 入口边 / 能力边 / 连线 → Edge 只许叫「下游」
//   - 机器 / 执行者                       → Node 只许叫「节点」
//   - 触发器                              → Source 只许叫「源」
//   - 事件中心                            → Event 只许叫「事件」（页面「事件流」）
//   - 下发 / 已应用                       → Deployment 只许叫「部署」
//   - 能力定义                            → Capability 只许叫「能力」
//   - 内联 spec                           → 用户看不到 spec，且「内联」只是形态
var zhBannedPhrases = []bannedPhrase{
	{"条边", "下游"},
	{"补边", "补下游"},
	{"入口边", "目标能力"},
	{"能力边", "下游"},
	{"连线", "下游"},
	{"机器", "节点"},
	{"触发器", "源"},
	{"事件中心", "事件流"},
	{"下发", "部署"},
	{"执行者", "节点"},
	{"能力定义", "能力"},
	{"内联 spec", "内联能力"},
}

// vocabWhitelist 是**具名**豁免：key → 短语 → 为什么这句不能改。
//
// 纪律：每一条都要说明「这里的短语不是那个概念」，而不是「这句懒得改」。
// 整类跳过（例如「所有 docs.* 都豁免」）是不允许的。
var vocabWhitelist = map[string]map[string]string{
	// 这里的「下发」是动词「告诉 / 派给节点」，不是 Deployment（能力 → 节点的下发）。
	// 整句在讲 R1 的那条边界：中枢只说做什么、永不说怎么做（没有 argv / shell）。
	// 换成「部署」会把一句协议描述变成一句错误描述，所以留原词。
	"docs.node.note": {
		"下发": "动词用法（中枢只说做什么/怎么做），不是 Deployment 概念",
	},
}

// enBannedPattern 是英文侧的禁词正则。
//
// 英文不需要禁「边」相关的汉字，但同样有「一个词两个意思」的问题：
// Edge 在界面上只许叫 Downstream。所以凡是独立的 edge/edges 一律判红。
var enBannedPattern = regexp.MustCompile(`(?i)\bedge(s)?\b`)

// enWhitelist 同上：key → 理由。
var enWhitelist = map[string]string{
	// 「no edge symbols」是在说用户名首尾不能有符号，这里的 edge 是「边缘」，
	// 与 Edge（下游）无关。
	"login.signup.username_hint": "edge = 首尾边缘符号，不是 Edge 概念",
	// 浏览器名字 Edge。改不了。
	"docs.webpush.android.desc": "Microsoft Edge 浏览器名",
}

// vocabPin 把关键标签的期望值写死（§13.4 的钉桩表）。
//
// 挑这些 key 的标准：它们是**七个词在界面上的落点**。改名必须同时改契约与这里，
// 所以任何一条变了，这个测试就是那次改动的第二位审阅者。
type vocabPin struct {
	key  string
	want map[string]string
}

var vocabPins = []vocabPin{
	// —— Capability / 能力 ——
	{"common.nav.capabilities", map[string]string{
		"zh-CN": "能力", "en": "Capabilities", "ja": "能力", "es": "Capacidades",
	}},
	{"pb.sec.capabilities", map[string]string{
		"zh-CN": "能力", "en": "Capabilities", "ja": "能力", "es": "Capacidades",
	}},
	{"pb.act.install_cap", map[string]string{
		"zh-CN": "安装能力", "en": "Install capability", "ja": "能力を導入", "es": "Instalar capacidad",
	}},
	{"pb.status.blocked", map[string]string{
		"zh-CN": "缺能力", "en": "Needs capability", "ja": "能力不足", "es": "Falta capacidad",
	}},
	// —— Source / 源 ——
	{"common.nav.sources", map[string]string{
		"zh-CN": "源", "en": "Sources", "ja": "ソース", "es": "Orígenes",
	}},
	{"pb.sec.triggers", map[string]string{
		"zh-CN": "源", "en": "Sources", "ja": "ソース", "es": "Orígenes",
	}},
	// —— Edge / 下游 ——
	{"pb.sec.edges", map[string]string{
		"zh-CN": "下游", "en": "Downstream", "ja": "下流", "es": "Destinos",
	}},
	{"docs.openapi.tag.edges", map[string]string{
		"zh-CN": "下游", "en": "Downstream", "ja": "下流", "es": "Destinos",
	}},
	{"caps.form.downstream", map[string]string{
		"zh-CN": "下游能力（候选）", "en": "Downstream capabilities (candidates)",
		"ja": "下流能力（候補）", "es": "Capacidades destino (candidatas)",
	}},
	{"caps.form.batch_created", map[string]string{
		"zh-CN": "已创建 {caps} 个能力 · {m} 条下游", "en": "Created {caps} capabilities · {m} downstream",
		"ja": "{caps} 個の能力を作成 · {m} 本の下流", "es": "Se crearon {caps} capacidades · {m} destinos",
	}},
	// —— Node / 节点 ——
	{"common.field.node", map[string]string{
		"zh-CN": "节点", "en": "Node", "ja": "ノード", "es": "Nodo",
	}},
	{"common.nav.nodes", map[string]string{
		"zh-CN": "节点", "en": "Nodes", "ja": "ノード", "es": "Nodos",
	}},
	// —— Deployment / 部署 ——
	{"caps.deploy.col", map[string]string{
		"zh-CN": "部署", "en": "Deployment", "ja": "デプロイ", "es": "Despliegue",
	}},
	{"caps.deploy.result_title", map[string]string{
		"zh-CN": "部署结果", "en": "Deployment result", "ja": "デプロイ結果", "es": "Resultado del despliegue",
	}},
	{"caps.deploy.pushed", map[string]string{
		"zh-CN": "已部署", "en": "Deployed", "ja": "デプロイ済み", "es": "Desplegado",
	}},
	{"node.artifacts.state_applied", map[string]string{
		"zh-CN": "已部署", "en": "Deployed", "ja": "デプロイ済み", "es": "Desplegada",
	}},
	{"node.artifacts.state_pending", map[string]string{
		"zh-CN": "部署中", "en": "Deploying", "ja": "デプロイ中", "es": "Desplegando",
	}},
	// —— Event / 事件 ——
	{"common.nav.events", map[string]string{
		"zh-CN": "事件流", "en": "Event Stream", "ja": "イベントストリーム", "es": "Flujo de eventos",
	}},
	{"message.back_events", map[string]string{
		"zh-CN": "返回事件流", "en": "Back to events", "ja": "イベント一覧に戻る", "es": "Volver a eventos",
	}},
	// —— Task / 任务 ——
	{"message.chain.count", map[string]string{
		"zh-CN": "共 {n} 个任务", "en": "{n} tasks", "ja": "{n} 個のタスク", "es": "{n} tareas",
	}},
	{"nodepage.recent", map[string]string{
		"zh-CN": "最近任务", "en": "Recent tasks", "ja": "最近のタスク", "es": "Tareas recientes",
	}},
	{"trace.stat.slowest", map[string]string{
		"zh-CN": "最慢任务", "en": "Slowest task", "ja": "最遅タスク", "es": "Tarea más lenta",
	}},
	// —— 「出口」只许表示 spec.outputs ——
	{"pb.cap.outputs", map[string]string{
		"zh-CN": "出口：{o}", "en": "Outputs: {o}", "ja": "出口：{o}", "es": "Salidas: {o}",
	}},
	{"caps.detail.outputs", map[string]string{
		"zh-CN": "出口（能发什么）", "en": "Outputs (what it can emit)",
		"ja": "出口（何を出せるか）", "es": "Salidas (qué puede emitir)",
	}},
	// —— 图的计数：一张卡是「能力」，不是「节点」（§13.2） ——
	{"graph.silk_meta_preview", map[string]string{
		"zh-CN": "{n} 个能力 · {m} 条下游", "en": "{n} capabilities · {m} downstream",
		"ja": "{n} 個の能力 · {m} 本の下流", "es": "{n} capacidades · {m} destinos",
	}},
	{"graph.hud_path", map[string]string{
		"zh-CN": "任务路径", "en": "Task path", "ja": "タスク経路", "es": "Ruta de tareas",
	}},
	// —— digest 的技术词处理（§13.1 外加项） ——
	{"node.artifacts.digest", map[string]string{
		"zh-CN": "内容指纹", "en": "content fingerprint",
		"ja": "内容フィンガープリント", "es": "huella de contenido",
	}},
}

// TestVocabulary 跑两条机制：禁词表 + 钉桩表。
func TestVocabulary(t *testing.T) {
	flat := map[string]map[string]string{}
	for _, lang := range vocabLangs {
		flat[lang] = localeStrings(t, lang)
	}

	// ---- ① 禁词表 ----
	t.Run("禁词表_zh", func(t *testing.T) {
		for key, val := range flat["zh-CN"] {
			allow := vocabWhitelist[key]
			for _, banned := range zhBannedPhrases {
				if !strings.Contains(val, banned.phrase) {
					continue
				}
				if _, ok := allow[banned.phrase]; ok {
					continue
				}
				t.Errorf("zh-CN %q 命中禁词 %q（应改用「%s」）\n  实际文案：%s",
					key, banned.phrase, banned.instead, val)
			}
		}
	})

	t.Run("禁词表_en", func(t *testing.T) {
		for key, val := range flat["en"] {
			if _, ok := enWhitelist[key]; ok {
				continue
			}
			if m := enBannedPattern.FindString(val); m != "" {
				t.Errorf("en %q 命中禁词 %q（Edge 在界面上只许叫 Downstream）\n  实际文案：%s",
					key, m, val)
			}
		}
	})

	// 白名单本身也要有维护：豁免的 key 不存在了就该删掉，否则它会悄悄
	// 变成下一句旧词的挡箭牌。
	t.Run("白名单不许过期", func(t *testing.T) {
		for key, phrases := range vocabWhitelist {
			val, ok := flat["zh-CN"][key]
			if !ok {
				t.Errorf("白名单里的 zh-CN key %q 已不存在，请删掉这条豁免", key)
				continue
			}
			for phrase, reason := range phrases {
				if reason == "" {
					t.Errorf("白名单 %q/%q 没写理由（每条豁免都必须说明为什么不能改）", key, phrase)
				}
				if !strings.Contains(val, phrase) {
					t.Errorf("白名单 %q 已经不再含 %q，这条豁免过期了", key, phrase)
				}
			}
		}
		for key, reason := range enWhitelist {
			if _, ok := flat["en"][key]; !ok {
				t.Errorf("en 白名单里的 key %q 已不存在，请删掉这条豁免", key)
			}
			if reason == "" {
				t.Errorf("en 白名单 %q 没写理由", key)
			}
		}
	})

	// ---- ② 钉桩表 ----
	t.Run("钉桩表", func(t *testing.T) {
		for _, pin := range vocabPins {
			for lang, want := range pin.want {
				got, ok := flat[lang][pin.key]
				if !ok {
					t.Errorf("%s 缺 key %q（钉桩表要求它是 %q）", lang, pin.key, want)
					continue
				}
				if got != want {
					t.Errorf("%s %q = %q，钉桩表要求 %q\n"+
						"  改名请同时改 docs/design/capability-spec-v3.md §13 与 internal/server/vocabulary_test.go",
						lang, pin.key, got, want)
				}
			}
		}
	})

	// ---- ③ 四个语言的 key 集合必须一致 ----
	// ja/es 少一个 key 时页面静默回落，同一句话就会在两个语言里说出两个词。
	t.Run("key_集合一致", func(t *testing.T) {
		base := flat["zh-CN"]
		for _, lang := range vocabLangs {
			if lang == "zh-CN" {
				continue
			}
			for key := range base {
				if _, ok := flat[lang][key]; !ok {
					t.Errorf("%s 缺 key %q（zh-CN 有）", lang, key)
				}
			}
			for key := range flat[lang] {
				if _, ok := base[key]; !ok {
					t.Errorf("%s 多出 key %q（zh-CN 没有）", lang, key)
				}
			}
		}
	})
}
