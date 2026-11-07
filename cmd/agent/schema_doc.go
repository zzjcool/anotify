package main

// 本文件是 `anotify-agent schema` 的三份**手写** JSON Schema（issue #319 §2）。
//
// 为什么不从 struct 反射生成、也不引 schema 生成库：
//
//   - 「不引构建期/运行期重依赖」是这个仓库的既有纪律（cobra 那条同理，见 usage.go）；
//   - 反射生成会把 Go 的类型细节（指针/零值/嵌入式 struct）一起漏给用户，而作者真正
//     要的是「这个字段怎么写、什么范围」；
//   - 手写才有地方写 description —— 一份没有说明的 JSON Schema 对写 yaml 的人只是
//     另一份要读的东西。
//
// 代价是「两份真理」会漂，所以配一条一致性守卫（schema_consistency_test.go）：
// 对 required / enum(const) / pattern 三类约束，schema 判合法而 artifact.Validate
// 拒绝（或反之）必须被测出来。改这里就要跑那条用例。
//
// 三份文档分别是：
//
//	node      node.yaml 的四个键（cmd/agent/config.go 的 nodeConfig + validate）
//	capability  能力 yaml（internal/artifact 的 Artifact/Metadata/Spec/Run + Validate）
//	emit      agent 起能力进程前下发的**出口契约**（$ANOTIFY_EMIT_PORTS / emit.json，
//	          见 docs/design/emit-protocol.md §2.1 与 cmd/agent/runner.go 的 emitPortContract）
//
// 写成原始 JSON 而不是 Go map 字面量：这些文档的主要读者是人（写 yaml 的人、改
// 一致性守卫的人），一份能直接贴进 ajv / 文档的 JSON 比 map[string]any{...} 好读得多。
// 输出时统一走 json.MarshalIndent（key 排序），所以**同一份代码两次输出逐字节相同**，
// 与原始串里的空白顺序无关（可以在 CI 里 diff）。

// 三份 schema 的名字（`anotify-agent schema <name>` 的位置参数）。
const (
	// schemaNode 是 node.yaml 的形状。
	schemaNode = "node"
	// schemaCapability 是能力 yaml 的形状（CLI 上叫 capability：产物 metadata.name 那个词）。
	schemaCapability = "capability"
	// schemaEmit 是出口契约（ANOTIFY_EMIT_PORTS / emit.json）的形状。
	schemaEmit = "emit"
)

// schemaDocNode 是 node.yaml 的 JSON Schema。
//
// 约束来源是 config.go 的 validate（**不是**凭印象）：server 必填且 scheme 必须是
// http/https/ws/wss（大小写不敏感）+ 必须有 host；token 必填；instances >= 0（<=0
// 归一为 1）；未知键拒绝（yaml 解码开了 KnownFields，所以拼错的键不会静默失效）。
//
// name 不required：config.validate 不检查它（现在只用于日志），声明成必填会让
// 「本来能起来的 node.yaml」在 schema 里被判非法。
const schemaDocNode = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "anotify node.yaml",
  "description": "节点 bootstrap 配置：只有四个键。未知键会被拒绝（拼错的字段名不会静默失效）。",
  "type": "object",
  "required": ["server", "token"],
  "additionalProperties": false,
  "properties": {
    "name": {
      "type": "string",
      "description": "节点名；可省略（目前只进日志与 hello 帧的展示）"
    },
    "server": {
      "type": "string",
      "pattern": "^([Hh][Tt][Tt][Pp][Ss]?|[Ww][Ss][Ss]?)://[^/?#]+",
      "description": "中枢地址，必须带 scheme（http/https/ws/wss，大小写不敏感）且带 host；无 scheme 不猜。agent 会自己把它映射到 /v1/node/stream"
    },
    "token": {
      "type": "string",
      "minLength": 1,
      "description": "节点令牌（nt_…），在控制台「加机器」页生成"
    },
    "instances": {
      "type": "integer",
      "minimum": 0,
      "description": "并发槽位数；省略或 0 = 1。它是节点自己的资源声明，中枢只能少用不能多用（裁决 D5）"
    }
  }
}`

// schemaDocCapability 是能力 yaml 的 JSON Schema。
//
// 字段与 internal/artifact 一一对应（apiVersion/kind/metadata/spec，spec 下
// displayName/params/outputs/requires/run），约束取自 Validate：
//
//	required   root 四件、metadata 的 name/version、spec.run、run 的 entrypoint/files
//	enum/const apiVersion = anotify/v1、kind = Capability
//	pattern    metadata.name、outputs[]、requires[]、run.env 的键
//	范围       version >= 1、timeoutSec 0..86400、outputs <= 32 个、requires <= 16 个
//
// 结构之外的约束（入口文件在 files 里、内容以 #! 开头、单文件 256 KiB、路径不含
// ..、env 不得覆盖 ANOTIFY_*）写成 description：它们要么依赖别的字段（entrypoint
// 必须是 files 的 key），要么是体积/路径语义，JSON Schema 表达不了而不失真。
const schemaDocCapability = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "anotify capability yaml（能力产物）",
  "description": "一份**自包含**的能力产物：参数契约 + 入口文件 + 全部文件内容 + 超时 + 环境变量 + 依赖的外部命令。控制台编译出来的就是它，手写的一样能跑（digest 变 = 本地变体）。",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "additionalProperties": false,
  "properties": {
    "apiVersion": {
      "const": "anotify/v1",
      "description": "格式版本；只支持 anotify/v1"
    },
    "kind": {
      "const": "Capability",
      "description": "唯一的 kind"
    },
    "metadata": {
      "type": "object",
      "required": ["name", "version"],
      "additionalProperties": false,
      "properties": {
        "name": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,62}$",
          "description": "能力名：小写字母、数字、点、下划线、短横线，1..63 字符，必须以字母或数字开头"
        },
        "version": {
          "type": "integer",
          "minimum": 1,
          "description": "版本号，>= 1（name@version 是中枢侧的唯一身份）"
        }
      }
    },
    "spec": {
      "type": "object",
      "required": ["run"],
      "additionalProperties": false,
      "properties": {
        "displayName": {
          "type": "string",
          "description": "给人看的名字（控制台列表用）；可省略"
        },
        "params": {
          "type": "object",
          "description": "参数契约：一份 JSON Schema 对象（properties/required 由控制台表单渲染）。它会归一成 key 有序，参与 digest。"
        },
        "outputs": {
          "type": "array",
          "maxItems": 32,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "pattern": "^[a-zA-Z0-9._-]{1,32}$"
          },
          "description": "出口名（封闭集合，不变量 I2）：能力能往下游发的东西只能是这些出口之一。可省略 = 终端能力（一次都不 emit 是合法的）。"
        },
        "requires": {
          "type": "array",
          "maxItems": 16,
          "items": {
            "type": "string",
            "pattern": "^[a-zA-Z0-9._+-]{1,64}$"
          },
          "description": "依赖的外部命令（如 gh、git）：只是声明，agent 不负责安装。"
        },
        "run": {
          "type": "object",
          "required": ["entrypoint", "files"],
          "additionalProperties": false,
          "properties": {
            "entrypoint": {
              "type": "string",
              "minLength": 1,
              "description": "入口文件：必须是 files 里的一个 key，且内容以 #! 开头（执行方式是直接 exec，不经 shell）。"
            },
            "files": {
              "type": "object",
              "minProperties": 1,
              "propertyNames": {
                "type": "string",
                "minLength": 1,
                "maxLength": 255
              },
              "additionalProperties": {
                "type": "string"
              },
              "description": "相对路径 → 文件内容。路径必须是 / 分隔的 POSIX 相对路径（不含空路径段、不含 . 或 ..、不含反斜杠、不含 NUL）；单文件上限 256 KiB、合计 1 MiB。"
            },
            "timeoutSec": {
              "type": "integer",
              "minimum": 0,
              "maximum": 86400,
              "description": "本地超时秒数；0 或省略 = 不设本地超时（交给中枢租约兜底）。上限 24 小时。"
            },
            "env": {
              "type": "object",
              "propertyNames": {
                "type": "string",
                "pattern": "^[A-Za-z_][A-Za-z0-9_]*$",
                "not": {
                  "pattern": "^[Aa][Nn][Oo][Tt][Ii][Ff][Yy]_"
                }
              },
              "additionalProperties": {
                "type": "string"
              },
              "description": "产物自带的环境变量；ANOTIFY_ 前缀保留给契约变量（ANOTIFY_PARAMS / ANOTIFY_DIGEST / ANOTIFY_EMIT_SOCK …），产物不得覆盖。"
            },
            "workDir": {
              "type": "string",
              "maxLength": 1024,
              "description": "执行目录：绝对路径，或 ~ / ~/…（由节点展开成 agent 用户的家目录）；空 = 每次任务一个独立目录。不含 .. 路径段。"
            }
          }
        }
      }
    }
  }
}`

// schemaDocEmit 是出口契约的 JSON Schema。
//
// 这份文档描述的是**下发给能力的**那一份：[{port,pick,schema,targets:[{…,needs}]}]
// （$ANOTIFY_EMIT_PORTS 与 $ANOTIFY_TASKDIR/emit.json 是同一份字节）。作者照着它拼
// data，就不必「被拒一次再猜」。三处口径必须一起看：
//
//	port      能力的 outputs[]（封闭集合）—— `pattern` 与 artifact.validateOutputs 同一套字符
//	pick      一次 emit 建几个下游任务（目前恒为 one：候选里选一个，不是扇出）
//	schema    不点名（--to）时的门槛：该端口所有候选要求的**并集**
//	needs     点名这个候选时的门槛：它自己还要哪些字段（null = 不需要任何字段）
//
// schema / needs 本身是 JSON Schema 片段，这里只声明「是对象或 null」，不深入描述它们
// 的内部（那是各自的 params 契约，属于中枢侧能力定义）。
const schemaDocEmit = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "anotify 出口契约（$ANOTIFY_EMIT_PORTS / $ANOTIFY_TASKDIR/emit.json）",
  "description": "agent 起能力进程前下发的出口契约（docs/design/emit-protocol.md §2.1）：同一个任务的变量与文件是**逐字节相同**的一份 JSON。顶层按 port 名升序，targets 按 (priority, capability, version) 升序；没有端口时是 []（不是 null）。",
  "type": "array",
  "items": {
    "$ref": "#/$defs/portContract"
  },
  "$defs": {
    "portName": {
      "type": "string",
      "pattern": "^[a-zA-Z0-9._-]{1,32}$",
      "description": "出口名（内部路由键）：必须属于本能力的 outputs[]（封闭集合 I2）。脚本不必知道它叫什么——emit 时用 --to 点名下游，端口由中枢的路由定"
    },
    "portContract": {
      "type": "object",
      "required": ["port", "pick", "schema", "targets"],
      "additionalProperties": false,
      "properties": {
        "port": {
          "$ref": "#/$defs/portName"
        },
        "pick": {
          "const": "one",
          "description": "一次 emit 建几个下游任务：目前恒为 one（同出口的多个下游是候选，只走一个；不是扇出）"
        },
        "schema": {
          "type": ["object", "null"],
          "description": "不点名（--to）时的门槛：这个端口所有候选要求的并集（一份 JSON Schema）。null = 这个端口没有下游，只做结构校验。"
        },
        "targets": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/target"
          },
          "description": "这个端口的候选下游；没有候选时是 []（不是 null）"
        }
      }
    },
    "target": {
      "type": "object",
      "required": ["capability", "version", "priority", "needs"],
      "additionalProperties": false,
      "properties": {
        "capability": {
          "type": "string",
          "description": "下游能力名：emit 时写 --to 的名字（可写 name@version 钉死版本）"
        },
        "version": {
          "type": "integer",
          "description": "下游能力的版本"
        },
        "priority": {
          "type": "integer",
          "description": "候选优先级（小的优先；0 = 默认）。选中谁由中枢在派发时定，这里的顺序只是确定性。"
        },
        "needs": {
          "type": ["object", "null"],
          "description": "点名这一个候选时它还要哪些字段（一份 JSON Schema）；null = 投给它不需要任何字段。"
        }
      }
    }
  }
}`

// schemaDoc 按名字取 schema 的原始 JSON。
//
// 名字是 CLI 的位置参数，所以这里同时是「可用的 schema 名」的唯一清单（-h 与错误
// 消息都从它生成）。
var schemaDoc = map[string]string{
	schemaNode:       schemaDocNode,
	schemaCapability: schemaDocCapability,
	schemaEmit:       schemaDocEmit,
}

// schemaNames 是 schema 名，顺序固定（-h 与错误消息里按这个顺序列）。
func schemaNames() []string {
	return []string{schemaNode, schemaCapability, schemaEmit}
}
