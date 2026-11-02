package artifact

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Template 是控制台的「新建能力」入口。
//
// 关键裁决（契约 D2）：模板是**代码生成器的输入**，不是执行器类型。
// v1 只有一种执行器——脚本。所谓「Agent 类型」只是预置的脚本 + 预置的表单字段。
// 想加一个新 Agent，加一个模板即可，不用改协议、不用发节点。
//
// 模板**不做文本插值**：脚本正文是固定的，用户填的字段值在运行时通过
// ANOTIFY_PARAM_* 环境变量进入脚本。这样模板层就没有任何注入面。
type Template struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"displayName"`
	Category    string  `json:"category"` // script | agent
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
	// Script 是入口文件 run.sh 的正文。
	Script string `json:"script"`
	// Requires 是机器上必须预装的外部命令（部署时探测，缺了部署失败）。
	Requires []string `json:"requires"`
	// Outputs 是建议的出口名。
	Outputs []string `json:"outputs"`
	// TimeoutSec 是默认超时。
	TimeoutSec int `json:"timeoutSec"`
}

// Field 是表单上的一个输入项。key 必须是 snake_case——
// 它会变成 ANOTIFY_PARAM_<UPPER>，而 paramEnvName 只做「大写 + 非字母数字换下划线」，
// 所以 camelCase 会变成不可猜的名字（见 CONTRACT-artifact §6）。
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // string | text | number | select
	Required bool     `json:"required"`
	Default  string   `json:"default"`
	Options  []string `json:"options,omitempty"` // Type == "select" 时有效
	Help     string   `json:"help"`
}

// RenderInput 是编译模板时的输入。
type RenderInput struct {
	// Name 是能力名（必填）。
	Name string
	// Version 缺省为 1。
	Version int
	// DisplayName 缺省用模板的 DisplayName。
	DisplayName string
	// TimeoutSec 为 0 时用模板默认。
	TimeoutSec int
	// Files 非空时整体覆盖模板生成的 files（控制台的「脚本模式」走这条路）。
	Files map[string]string
	// Entrypoint 在 Files 被覆盖时指定入口；空则用 "run.sh"。
	Entrypoint string
}

var templates = []Template{
	{
		ID:          "script.blank",
		DisplayName: "空白脚本",
		Category:    "script",
		Description: "从零开始写一段 bash。参数通过 ANOTIFY_PARAM_* 环境变量读取。",
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

echo "capability: $ANOTIFY_CAPABILITY"
echo "digest:     $ANOTIFY_DIGEST"
echo "params:     $ANOTIFY_PARAMS"
`,
		TimeoutSec: 900,
	},
	{
		ID:          "script.http",
		DisplayName: "HTTP 请求",
		Category:    "script",
		Description: "按参数发一个 HTTP 请求，输出响应体。",
		Fields: []Field{
			{Key: "url", Label: "URL", Type: "string", Required: true, Help: "完整地址，含 scheme"},
			{Key: "method", Label: "方法", Type: "select", Default: "POST",
				Options: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
			{Key: "body", Label: "请求体", Type: "text", Help: "JSON 文本；留空发送 {}"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

curl -fsS -X "${ANOTIFY_PARAM_METHOD:-POST}" \
  -H 'content-type: application/json' \
  --data "${ANOTIFY_PARAM_BODY:-{\}}" \
  "$ANOTIFY_PARAM_URL"
`,
		Requires:   []string{"curl"},
		TimeoutSec: 120,
	},
	{
		ID:          "script.git",
		DisplayName: "拉取 Git 仓库",
		Category:    "script",
		Description: "浅克隆一个仓库到工作目录并输出最近一次提交。",
		Fields: []Field{
			{Key: "repo", Label: "仓库地址", Type: "string", Required: true, Help: "https 或 ssh 地址"},
			{Key: "ref", Label: "分支 / 标签", Type: "string", Help: "留空用默认分支"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

args=(--depth 1)
if [ -n "${ANOTIFY_PARAM_REF:-}" ]; then
  args+=(--branch "$ANOTIFY_PARAM_REF")
fi
git clone "${args[@]}" "$ANOTIFY_PARAM_REPO" work
cd work
git log -1 --stat
`,
		Requires:   []string{"git"},
		TimeoutSec: 900,
	},
	{
		ID:          "script.test",
		DisplayName: "跑测试",
		Category:    "script",
		Description: "在工作目录里执行一条测试命令。退出码非 0 视为任务失败。",
		Fields: []Field{
			{Key: "command", Label: "测试命令", Type: "string", Required: true, Default: "make test"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

# 注意：这里的命令来自能力参数，是能力作者自己给的——它就是要执行任意命令。
# 契约 I1 管的是「中枢不得执行」，不是「脚本里不许调 shell」。
bash -c "$ANOTIFY_PARAM_COMMAND"
`,
		TimeoutSec: 1800,
	},
	{
		ID:          "script.notify",
		DisplayName: "回调 Webhook",
		Category:    "script",
		Description: "把标题和正文 POST 到一个 webhook 地址。",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook 地址", Type: "string", Required: true},
			{Key: "title", Label: "标题", Type: "string"},
			{Key: "body", Label: "正文", Type: "text"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

payload=$(jq -n \
  --arg t "${ANOTIFY_PARAM_TITLE:-}" \
  --arg b "${ANOTIFY_PARAM_BODY:-}" \
  '{title: $t, body: $b}')

curl -fsS -X POST -H 'content-type: application/json' --data "$payload" \
  "$ANOTIFY_PARAM_WEBHOOK_URL"
`,
		Requires:   []string{"curl", "jq"},
		TimeoutSec: 120,
	},
	{
		ID:          "agent.claude",
		DisplayName: "Claude Code",
		Category:    "agent",
		Description: "用 Claude Code 执行一段提示词。密钥留在机器上（机器需已装 llm 与登录态）。",
		Fields: []Field{
			{Key: "prompt", Label: "提示词", Type: "text", Required: true},
			{Key: "model", Label: "模型", Type: "string", Help: "留空用默认"},
			{Key: "tools", Label: "允许的工具", Type: "string", Help: "逗号分隔，留空用默认"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

args=(--print)
if [ -n "${ANOTIFY_PARAM_MODEL:-}" ]; then
  args+=(--model "$ANOTIFY_PARAM_MODEL")
fi
if [ -n "${ANOTIFY_PARAM_TOOLS:-}" ]; then
  args+=(--allowedTools "$ANOTIFY_PARAM_TOOLS")
fi
claude "${args[@]}" "$ANOTIFY_PARAM_PROMPT"
`,
		Requires:   []string{"claude"},
		TimeoutSec: 3600,
	},
	{
		ID:          "agent.codex",
		DisplayName: "Codex",
		Category:    "agent",
		Description: "用 Codex CLI 执行一段提示词。密钥留在机器上。",
		Fields: []Field{
			{Key: "prompt", Label: "提示词", Type: "text", Required: true},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

codex exec "$ANOTIFY_PARAM_PROMPT"
`,
		Requires:   []string{"codex"},
		TimeoutSec: 3600,
	},
	{
		ID:          "agent.pi",
		DisplayName: "PI",
		Category:    "agent",
		Description: "用 PI 执行一段提示词。密钥留在机器上。",
		Fields: []Field{
			{Key: "prompt", Label: "提示词", Type: "text", Required: true},
			{Key: "model", Label: "模型", Type: "string", Help: "留空用默认"},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

if [ -n "${ANOTIFY_PARAM_MODEL:-}" ]; then
  pi --print -m "$ANOTIFY_PARAM_MODEL" -p "$ANOTIFY_PARAM_PROMPT"
else
  pi --print -p "$ANOTIFY_PARAM_PROMPT"
fi
`,
		Requires:   []string{"pi"},
		TimeoutSec: 3600,
	},
	{
		ID:          "agent.custom",
		DisplayName: "自定义 Agent CLI",
		Category:    "agent",
		Description: "调用任意已安装在机器上的 Agent 命令行。",
		Fields: []Field{
			{Key: "agent_command", Label: "命令名", Type: "string", Required: true,
				Help: "例如 aider / amp / cursor-agent"},
			{Key: "prompt", Label: "提示词", Type: "text", Required: true},
		},
		Script: `#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"

"$ANOTIFY_PARAM_AGENT_COMMAND" "$ANOTIFY_PARAM_PROMPT"
`,
		TimeoutSec: 3600,
	},
}

// Templates 返回全部内置模板（深拷贝，调用方可安全修改）。
func Templates() []Template {
	out := make([]Template, len(templates))
	copy(out, templates)
	for i := range out {
		// 空列表一律给 []（不变量 I9）：`null` 会让前端把「没有字段」误判成
		// 「字段没加载出来」，而且控制台是直接 JSON.parse 后遍历的。
		out[i].Fields = append(make([]Field, 0, len(templates[i].Fields)), templates[i].Fields...)
		for j := range out[i].Fields {
			out[i].Fields[j].Options = append(make([]string, 0, len(templates[i].Fields[j].Options)),
				templates[i].Fields[j].Options...)
		}
		out[i].Requires = append(make([]string, 0, len(templates[i].Requires)), templates[i].Requires...)
		out[i].Outputs = append(make([]string, 0, len(templates[i].Outputs)), templates[i].Outputs...)
	}
	return out
}

// LookupTemplate 按 ID 取模板。
func LookupTemplate(id string) (Template, bool) {
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// Render 把模板编译成一份产物。
//
// 走「脚本模式」时调用方把 Files 填进来，模板的脚本体被整体覆盖；
// 否则用模板自带的脚本体。
func Render(templateID string, in RenderInput) (*Artifact, error) {
	tpl, ok := LookupTemplate(templateID)
	if !ok {
		return nil, &Error{Problems: []string{
			fmt.Sprintf("unknown_template: 没有 id 为 %q 的模板", templateID),
		}}
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, &Error{Problems: []string{"invalid_name: 能力名必填"}}
	}

	version := in.Version
	if version == 0 {
		version = 1
	}

	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = tpl.DisplayName
	}

	timeout := in.TimeoutSec
	if timeout == 0 {
		timeout = tpl.TimeoutSec
	}

	files := in.Files
	entrypoint := in.Entrypoint
	if len(files) == 0 {
		files = map[string]string{"run.sh": tpl.Script}
		entrypoint = "run.sh"
	}
	if entrypoint == "" {
		entrypoint = "run.sh"
	}

	params, err := paramsSchemaFor(tpl.Fields)
	if err != nil {
		return nil, err
	}

	a := &Artifact{
		APIVersion: APIVersion,
		Kind:       KindCapability,
		Metadata:   Metadata{Name: name, Version: version},
		Spec: Spec{
			DisplayName: displayName,
			Params:      params,
			Outputs:     append([]string{}, tpl.Outputs...),
			Requires:    append([]string{}, tpl.Requires...),
			Run: Run{
				Entrypoint: entrypoint,
				Files:      files,
				TimeoutSec: timeout,
			},
		},
	}
	if err := Validate(a); err != nil {
		return nil, err
	}
	return a, nil
}

// paramsSchemaFor 从表单字段推导参数契约。
//
// 由字段推导而不是让模板各写一份：字段与 schema 永远不可能漂移。
func paramsSchemaFor(fields []Field) (json.RawMessage, error) {
	props := make(map[string]any, len(fields))
	required := make([]string, 0, len(fields))

	for _, f := range fields {
		if !isSnakeCase(f.Key) {
			return nil, &Error{Problems: []string{
				fmt.Sprintf("invalid_field: 字段名 %q 必须是 snake_case"+
					"（它会变成 ANOTIFY_PARAM_<UPPER>）", f.Key),
			}}
		}
		prop := map[string]any{}
		switch f.Type {
		case "number":
			prop["type"] = "number"
		case "select":
			prop["type"] = "string"
			prop["enum"] = append([]string{}, f.Options...)
		default: // string | text
			prop["type"] = "string"
		}
		props[f.Key] = prop
		if f.Required {
			required = append(required, f.Key)
		}
	}

	sort.Strings(required)
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// isSnakeCase 判断一个字段名是否是 snake_case。
func isSnakeCase(s string) bool {
	if s == "" || s[0] == '_' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
