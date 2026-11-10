package protocol

import "strings"

// 任务环境里的 ANOTIFY_ 契约变量（capability.md §4.0 / A2）。
//
// 这张表就是**全部** ANOTIFY_ 变量，一个不多：能力脚本只靠这些名字拿到本次
// 任务的端点、令牌、任务 id、参数与导出目录。守护进程注入，中枢从不直接下发
// 它们；名字是协议的一部分，节点/中枢/能力三方都从这里取，禁止各写一份字面量。
const (
	// EnvEmitEndpoint 是本次任务的 emit 端点（协议 + 地址 + 端口，如
	// `http://127.0.0.1:41237`）。脚本拼 `/emit` 后 POST。
	EnvEmitEndpoint = "ANOTIFY_EMIT_ENDPOINT"
	// EnvTaskToken 是本次任务的高熵令牌：`Authorization: Bearer <token>`。
	// 它按 token → task 归属请求，不进 argv、不进 payload。
	EnvTaskToken = "ANOTIFY_TASK_TOKEN"
	// EnvTaskID 是本次任务 id（只读，用于日志关联）。
	EnvTaskID = "ANOTIFY_TASK_ID"
	// EnvParamPrefix 是任务参数变量的前缀（`ANOTIFY_PARAM_<NAME>`）。
	EnvParamPrefix = "ANOTIFY_PARAM_"
	// EnvArtifactDir 是本 attempt 专属的系统自有导出目录（A2，绝对路径）。
	//
	// 节点在系统结果目录下为**每个 (taskId, attempt)** 新建唯一随机后缀目录
	// （0700）并注入；脚本把要导出托管的普通文件写进去，节点完成后安全枚举、
	// 复制、上传。它只读、不可由 run.env / envPassthrough 覆盖（保留 ANOTIFY_
	// 规则），不进 vars/配置 hash/中枢环境值快照。空目录 = 真正没有导出文件。
	EnvArtifactDir = "ANOTIFY_ARTIFACT_DIR"
)

// ParamEnvName 是参数名 → 任务环境变量名的归一函数（capability.md §2.3）：
// 全大写、非 [A-Z0-9] 的字符替换成 '_'，**不插大小写边界**。
//
//	sha       → ANOTIFY_PARAM_SHA
//	repo-url  → ANOTIFY_PARAM_REPO_URL
//	repoUrl   → ANOTIFY_PARAM_REPOURL
//	参数      → ANOTIFY_PARAM__（每个 rune 一个 '_'）
//
// 归一可能撞名（`sha` / `SHA` / `Sha`，`repo-url` / `repo.url`）：**撞名不在这里
// 报错**，写入期由 artifact 用同一个函数检测并拒绝（param_env_collision）。
// 只有一份规则：artifact 的校验与节点的注入必须调用本函数，禁止各写一份。
func ParamEnvName(name string) string {
	var b strings.Builder
	b.Grow(len(EnvParamPrefix) + len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return EnvParamPrefix + b.String()
}
