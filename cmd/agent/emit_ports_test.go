package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖契约 §5 的「出口契约下发」：
//
//	ANOTIFY_EMIT_PORTS=<紧凑 JSON>   始终存在（没端口时是 []，不是空串也不是 null）
//	$ANOTIFY_TASKDIR/emit.json       同一份字节，0600，始终存在（任务目录每 attempt 清空重建）
//
// 两条硬要求：
//
//  1. **同源**：端口集合与 emit socket 的白名单来自同一个 helper（emitPortsOf），
//     expects 原样来自 assign 帧——节点**不做第二次推导**。两份各算一次的症状是
//     「脚本照着契约 emit，却被自己的节点以 exit 4 打回来」。
//  2. **失败可见**：契约文件写不下去时任务必须 failed、错误里带路径。静默跳过会让
//     脚本看到「有变量、没文件」这种一半的契约。
//
// JSON 形状（逐字）：
//
//	[{"port":"fix","pick":"one","schema":{"type":"object",…},"targets":[{"capability":"b","version":2,"priority":0,"needs":null}]}]
//
// 顶层按 port 名升序、targets 按 (priority, capability, version) 升序；schema 为 nil 时
// 是显式的 null（不是缺键），targets 为空时是 []（不是 null），每个 target 的 needs 为 nil
// 时同样是显式 null（= 投给它不需要任何字段）。
//
// pick / needs 是**给作者用的**（脚本 / LLM 照着拼参数）：
//
//	pick            一次 emit 建几个下游任务（目前恒为 one：同出口的多个下游是候选，不是扇出）
//	schema          不点名（--to）时的门槛：所有候选的并集
//	targets[].needs 点名这个候选时的门槛：它自己还差哪些字段
//
// 注意 SEAM(A)：targets[].priority 由 A 组的 edges.priority 竖切落地，本文件**不**
// 涉及那个字段（落地后排序口径会变成 (priority, capability)，见 runner.go 的注释）。

// ---------- 小工具 ----------

// envLookup 从 buildEnv 的结果里取一个契约变量的值（不存在时 ok=false）。
func envLookup(env []string, name string) (string, bool) {
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok && key == name {
			return value, true
		}
	}
	return "", false
}

// mustReadFile 读一个文件（失败即 Fatal）。
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return b
}

// ---------- ① 多端口 + 多 targets ----------

// TestBuildEnvEmitPorts：契约 §5 的逐字形状、确定顺序、以及「env 与文件同一份字节」。
func TestBuildEnvEmitPorts(t *testing.T) {
	workDir := t.TempDir()
	j := job{
		TaskID: "tsk_ports", Capability: "a", Version: 1,
		// 乱序 + 重复 + 带空白：证明输出的集合与顺序无关（归一后按 port 名升序）。
		Ports: []string{" review", "fix", "fix ", "review"},
		Expects: map[string]ws.PortExpect{
			"fix": {
				Schema: json.RawMessage(`{"type":"object","required":["issue"]}`),
				// 倒序 + 同 capability 不同 Version：证明 targets 按 (capability, version) 升序。
				// Needs 只给 c@1，其余留 nil → 同一份 JSON 里 "needs":{…} 与 "needs":null 都要出现
				// （null 是「投给它不需要字段」，不是「没下发」）。
				Targets: []ws.ExpectTarget{
					{Capability: "c", Version: 1,
						Needs: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`)},
					{Capability: "b", Version: 2},
					{Capability: "b", Version: 1},
				},
			},
			"review": {
				Schema:  json.RawMessage(`{"type":"object"}`),
				Targets: []ws.ExpectTarget{{Capability: "d", Version: 3}, {Capability: "a", Version: 1}},
			},
			// 不在 j.Ports 里 → **不许**输出（端口是封闭集合，I2）。
			"ghost": {Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	env, err := buildEnv(nil, j, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got, ok := envLookup(env, envEmitPorts)
	if !ok {
		t.Fatalf("环境里没有 %s（契约 §5 要求它始终存在）", envEmitPorts)
	}
	want := `[{"port":"fix","pick":"one","schema":{"type":"object","required":["issue"]},"targets":` +
		`[{"capability":"b","version":1,"priority":0,"needs":null},` +
		`{"capability":"b","version":2,"priority":0,"needs":null},` +
		`{"capability":"c","version":1,"priority":0,"needs":` +
		`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}}]},` +
		`{"port":"review","pick":"one","schema":{"type":"object"},"targets":` +
		`[{"capability":"a","version":1,"priority":0,"needs":null},` +
		`{"capability":"d","version":3,"priority":0,"needs":null}]}]`
	if got != want {
		t.Fatalf("ANOTIFY_EMIT_PORTS:\n got %s\nwant %s", got, want)
	}

	// 文件：落在 workDir 下、内容与 env 变量**字节一致**、权限 0600。
	emitPath := filepath.Join(workDir, emitContractFile)
	raw := mustReadFile(t, emitPath)
	if !bytes.Equal(raw, []byte(got)) {
		t.Fatalf("emit.json 与 ANOTIFY_EMIT_PORTS 不是同一份字节:\nfile=%s\nenv =%s", raw, got)
	}
	st, err := os.Stat(emitPath)
	if err != nil {
		t.Fatalf("stat %s: %v", emitPath, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s 权限 = %04o, want 0600", emitContractFile, perm)
	}

	// 再逐字段解一遍：字节比对说不清「哪个字段错了」。
	type target struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	}
	type contract struct {
		Port    string          `json:"port"`
		Schema  json.RawMessage `json:"schema"`
		Targets []target        `json:"targets"`
	}
	var decoded []contract
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("emit.json 不是合法 JSON: %v (%s)", err, raw)
	}
	if len(decoded) != 2 {
		t.Fatalf("端口数 = %d, want 2（ghost 不在 ports 里，不许输出）: %s", len(decoded), raw)
	}
	if decoded[0].Port != "fix" || decoded[1].Port != "review" {
		t.Fatalf("端口顺序 = [%s %s], want [fix review]", decoded[0].Port, decoded[1].Port)
	}
	if got := string(decoded[0].Schema); got != `{"type":"object","required":["issue"]}` {
		t.Fatalf("fix.schema = %s", got)
	}
	wantTargets := []target{{"b", 1}, {"b", 2}, {"c", 1}}
	if !reflect.DeepEqual(decoded[0].Targets, wantTargets) {
		t.Fatalf("fix.targets = %+v, want %+v", decoded[0].Targets, wantTargets)
	}
	if got := string(decoded[1].Schema); got != `{"type":"object"}` {
		t.Fatalf("review.schema = %s", got)
	}
}

// ---------- ② 空端口 ----------

// TestBuildEnvEmitPortsEmpty：没有端口没有 expects → 变量与文件都是 []（不是 ""、不是 null）。
func TestBuildEnvEmitPortsEmpty(t *testing.T) {
	workDir := t.TempDir()
	env, err := buildEnv(nil, job{TaskID: "t", Capability: "c", Version: 1}, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got, ok := envLookup(env, envEmitPorts)
	if !ok {
		t.Fatalf("环境里没有 %s（契约 §5 要求它始终存在）", envEmitPorts)
	}
	if got != "[]" {
		t.Fatalf("ANOTIFY_EMIT_PORTS = %q, want []（空端口是空数组，不是空串/null）", got)
	}
	if raw := mustReadFile(t, filepath.Join(workDir, emitContractFile)); string(raw) != "[]" {
		t.Fatalf("emit.json = %q, want []", raw)
	}
}

// ---------- ③ schema 为 null ----------

// TestBuildEnvEmitPortsSchemaNull：expects 里没有条目 / Schema 为 nil → "schema":null，
// 而且这个键**存在**（缺键会把「没有约束」说成「忘了下发」）。
func TestBuildEnvEmitPortsSchemaNull(t *testing.T) {
	workDir := t.TempDir()
	j := job{
		TaskID: "t", Capability: "c", Version: 1,
		Ports: []string{"no-entry", "nil-schema"},
		Expects: map[string]ws.PortExpect{
			// Schema 显式为 nil（有条目、有 targets）。
			"nil-schema": {Targets: []ws.ExpectTarget{{Capability: "b", Version: 2}}},
			// "no-entry" 在 expects 里**根本没有**条目。
			// "with-schema" 是对照组：证明 null 不是「全都变 null」。
			"with-schema": {Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	env, err := buildEnv(nil, j, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got, _ := envLookup(env, envEmitPorts)
	want := `[{"port":"nil-schema","pick":"one","schema":null,` +
		`"targets":[{"capability":"b","version":2,"priority":0,"needs":null}]},` +
		`{"port":"no-entry","pick":"one","schema":null,"targets":[]}]`
	if got != want {
		t.Fatalf("ANOTIFY_EMIT_PORTS:\n got %s\nwant %s", got, want)
	}

	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("ANOTIFY_EMIT_PORTS 不是合法 JSON: %v (%s)", err, got)
	}
	if len(decoded) != 2 {
		t.Fatalf("端口数 = %d, want 2: %s", len(decoded), got)
	}
	wantTargets := []string{`[{"capability":"b","version":2,"priority":0,"needs":null}]`, `[]`}
	for i, item := range decoded {
		schema, ok := item["schema"]
		if !ok {
			t.Fatalf("缺 schema 键（必须是显式 null）: %s", got)
		}
		if string(schema) != "null" {
			t.Fatalf("schema = %s, want null: %s", schema, got)
		}
		targets, ok := item["targets"]
		if !ok {
			t.Fatalf("缺 targets 键: %s", got)
		}
		if string(targets) != wantTargets[i] {
			t.Fatalf("第 %d 项 targets = %s, want %s（空候选是空数组，不是 null）: %s",
				i, targets, wantTargets[i], got)
		}
	}
}

// ---------- ④ 写不下去必须可见 ----------

// TestBuildEnvEmitPortsWriteFails：契约文件写不进去 → buildEnv 报错（带路径），
// 经 runJob 变成**可见的任务失败**，而且绝不能静默继续跑脚本。
func TestBuildEnvEmitPortsWriteFails(t *testing.T) {
	// ① 任务目录不存在 → 错误里必须有 emit.json 的**完整路径**（运维要知道去哪儿看）。
	missing := filepath.Join(t.TempDir(), "no-such-taskdir")
	emitPath := filepath.Join(missing, emitContractFile)
	if _, err := buildEnv(nil, job{TaskID: "t"}, missing, missing, ""); err == nil {
		t.Fatal("buildEnv: 写不了 emit.json 时必须返回错误（不许静默跳过）")
	} else if !strings.Contains(err.Error(), emitPath) {
		t.Fatalf("错误里没有 %s 的路径 %q: %v", emitContractFile, emitPath, err)
	}

	// ② 同一个失败经 runJob 必须是任务失败。造一个「emit.json 这个位置已被目录占住」
	// 的产物：expandFiles 先建出那个目录，于是 buildEnv 落盘时必然失败（rename 到
	// 目录上）。这比 mock 更接近真实场景（路径被别的文件/目录占了）。
	art := testScriptArtifact("demo.ports", 1, bashScript("echo should-not-run"))
	art.Spec.Run.Files[emitContractFile+"/keep"] = "occupied\n"

	res := runJob(context.Background(), job{
		TaskID: "tsk_emit_write_fail", Capability: "demo.ports", Version: 1,
		Artifact: art, WorkRoot: filepath.Join(t.TempDir(), "work"),
	})
	if res.Status != statusFailed {
		t.Fatalf("status = %q（err=%q），期望 failed：契约文件写不下去不能当成功", res.Status, res.Err)
	}
	if !strings.Contains(res.Err, emitContractFile) {
		t.Fatalf("Err = %q，期望带出 %s 路径（失败要能一眼看出是哪儿）", res.Err, emitContractFile)
	}
	if strings.Contains(res.Stdout, "should-not-run") {
		t.Fatalf("契约写失败后仍然执行了脚本：stdout=%q（这会把「没拿到契约」藏起来）", res.Stdout)
	}
}

// ---------- ⑤ 同源：来自 assign 帧那份，不重算 ----------

// TestBuildEnvEmitPortsSameSourceAsExpects：job.Ports/job.Expects 就是 assign 帧那份，
// ANOTIFY_EMIT_PORTS 恰好等于由它们序列化的结果；顺带吃掉 emitPortsOf 的 fallback 分支。
func TestBuildEnvEmitPortsSameSourceAsExpects(t *testing.T) {
	art := testScriptArtifact("a", 1, bashScript("true"))
	// 产物自己的 outputs 与 assign 的 ports **不同**：谁被用了必须一眼可辨。
	art.Spec.Outputs = []string{"from-outputs"}

	f := &assignFrame{
		Type: frameTypeAssign, TaskID: "tsk_same", Capability: "a", Version: 1,
		Ports: []string{"fix"},
		Expects: map[string]ws.PortExpect{
			"fix": {
				Schema:  json.RawMessage(`{"type":"object"}`),
				Targets: []ws.ExpectTarget{{Capability: "b", Version: 2}},
			},
			// 不在 ports 里：不输出（再次确认 expects 不是「照抄一份」）。
			"other": {Schema: json.RawMessage(`{"type":"object","required":["x"]}`)},
		},
	}

	ports := emitPortsOf(f, art)
	if !reflect.DeepEqual(ports, f.Ports) {
		t.Fatalf("emitPortsOf = %v, want assign 的 ports %v（有 ports 时不许退到 spec.outputs）",
			ports, f.Ports)
	}

	j := job{
		TaskID: f.TaskID, Capability: f.Capability, Version: f.Version,
		Artifact: art, Ports: ports, Expects: f.Expects,
	}
	workDir := t.TempDir()
	env, err := buildEnv(nil, j, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got, _ := envLookup(env, envEmitPorts)
	// 期望值直接由 j.Expects + j.Ports 手写（不经过被测量的那个函数）。
	want := `[{"port":"fix","pick":"one","schema":{"type":"object"},` +
		`"targets":[{"capability":"b","version":2,"priority":0,"needs":null}]}]`
	if got != want {
		t.Fatalf("ANOTIFY_EMIT_PORTS:\n got %s\nwant %s", got, want)
	}
	if raw := mustReadFile(t, filepath.Join(workDir, emitContractFile)); string(raw) != want {
		t.Fatalf("emit.json = %s, want %s", raw, want)
	}

	// fallback 分支：assign 没有 ports（旧中枢 / 空快照）→ 退到产物的 spec.outputs。
	fallback := emitPortsOf(&assignFrame{Type: frameTypeAssign, TaskID: "tsk_same"}, art)
	if !reflect.DeepEqual(fallback, art.Spec.Outputs) {
		t.Fatalf("emitPortsOf(fallback) = %v, want 产物的 spec.outputs %v", fallback, art.Spec.Outputs)
	}
}

// ---------- ⑥ 端到端：真进程看到的两个东西 ----------

// TestRunJobEmitPortsProbe：真起一个 bash 能力，把 $ANOTIFY_EMIT_PORTS 与
// $ANOTIFY_TASKDIR/emit.json 都抄出来 —— 两份必须相等，且字段齐。
//
// 变量里**就是** JSON 文本（不是文件路径）；文件是 $ANOTIFY_TASKDIR/emit.json。
//
// 探针落在任务目录**外面**：runJob 成功后会把任务目录删掉（契约 §5 的清理规则），
// 写进去的证据会跟着没。脚本读的仍然是 $ANOTIFY_TASKDIR/emit.json。
func TestRunJobEmitPortsProbe(t *testing.T) {
	probeDir := t.TempDir()
	fromEnvPath := filepath.Join(probeDir, "probe-ports.json")
	fromFilePath := filepath.Join(probeDir, "probe-emit.json")
	body := fmt.Sprintf(`printf '%%s' "$ANOTIFY_EMIT_PORTS" > %q
cat "$ANOTIFY_TASKDIR/emit.json" > %q
`, fromEnvPath, fromFilePath)

	j := newRunnerJob(t, body)
	j.Ports = []string{"review", "fix"}
	j.Expects = map[string]ws.PortExpect{
		"fix": {
			Schema:  json.RawMessage(`{"type":"object","required":["issue"]}`),
			Targets: []ws.ExpectTarget{{Capability: "b", Version: 2}},
		},
		"review": {Schema: json.RawMessage(`{"type":"object"}`)},
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q（err=%q stdout=%q stderr=%q），期望 succeeded", res.Status, res.Err, res.Stdout, res.Stderr)
	}

	fromEnv := mustReadFile(t, fromEnvPath)
	fromFile := mustReadFile(t, fromFilePath)
	if !bytes.Equal(fromEnv, fromFile) {
		t.Fatalf("ANOTIFY_EMIT_PORTS 与 emit.json 不是同一份字节:\nenv =%s\nfile=%s", fromEnv, fromFile)
	}
	if !json.Valid(fromEnv) {
		t.Fatalf("ANOTIFY_EMIT_PORTS 不是 JSON（它应该是 JSON 文本，不是文件路径）: %q", fromEnv)
	}

	// 字段齐：port / schema / targets / capability / version。
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(fromEnv, &decoded); err != nil {
		t.Fatalf("emit 契约不是 JSON 数组: %v (%s)", err, fromEnv)
	}
	if len(decoded) != 2 {
		t.Fatalf("端口数 = %d, want 2: %s", len(decoded), fromEnv)
	}
	wantPorts := []string{"fix", "review"} // 顶层按 port 名升序
	for i, item := range decoded {
		for _, key := range []string{"port", "schema", "targets"} {
			if _, ok := item[key]; !ok {
				t.Fatalf("第 %d 项缺 %q 键: %s", i, key, fromEnv)
			}
		}
		var port string
		if err := json.Unmarshal(item["port"], &port); err != nil {
			t.Fatalf("port 不是字符串: %v (%s)", err, item["port"])
		}
		if port != wantPorts[i] {
			t.Fatalf("第 %d 个端口 = %q, want %q（顶层按 port 名升序）", i, port, wantPorts[i])
		}
	}

	var targets []struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	}
	if err := json.Unmarshal(decoded[0]["targets"], &targets); err != nil {
		t.Fatalf("fix.targets: %v", err)
	}
	if len(targets) != 1 || targets[0].Capability != "b" || targets[0].Version != 2 {
		t.Fatalf("fix.targets = %+v, want [{b 2}]", targets)
	}
	if got := string(decoded[1]["schema"]); got != `{"type":"object"}` {
		t.Fatalf("review.schema = %s", got)
	}
}

// ---------- ⑦ emitPortsOf 的 fallback ----------

// TestEmitPortsOfFallsBackToArtifactOutputs：assign 无 ports → 退到 spec.outputs；
// 有 ports → 用 assign 的。它是 emit socket 白名单与 ANOTIFY_EMIT_PORTS 的**唯一**
// 入口（client.go 的两处调用），所以这里直接断言 helper 的结果。
func TestEmitPortsOfFallsBackToArtifactOutputs(t *testing.T) {
	art := testScriptArtifact("a", 1, bashScript("true"))
	art.Spec.Outputs = []string{"clean", "needs-fix"}

	// 无 ports（旧中枢 / 空快照）→ 产物的 spec.outputs（同样是封闭集合，I2 的源头）。
	if got := emitPortsOf(&assignFrame{Type: frameTypeAssign, TaskID: "tsk_fb"}, art); !reflect.DeepEqual(got, art.Spec.Outputs) {
		t.Fatalf("emitPortsOf(无 ports) = %v, want %v", got, art.Spec.Outputs)
	}
	// 有 ports → 用 assign 那份（中枢算的快照优先）。
	if got := emitPortsOf(&assignFrame{Type: frameTypeAssign, TaskID: "t", Ports: []string{"p"}}, art); !reflect.DeepEqual(got, []string{"p"}) {
		t.Fatalf("emitPortsOf(有 ports) = %v, want [p]", got)
	}
	// 既没有 ports 也没有产物 → 空（不 panic；buildEnv 会输出 []）。
	if got := emitPortsOf(&assignFrame{Type: frameTypeAssign, TaskID: "t"}, nil); got != nil {
		t.Fatalf("emitPortsOf(无 ports, 无产物) = %v, want nil", got)
	}
}

// ---------- ⑧ 作者视角：每个候选各自的 needs ----------

// TestBuildEnvEmitPortsCandidatesCarryOwnNeeds 复现开发环境那条事故的拓扑
// （trace_1793986152223364273_3611c934350f8618），断言**作者手里的说明书**足以
// 做出正确决定：
//
//	haha/out → script.echo（要 message，可选）
//	haha/out → waoo       （要 waoo，必填）
//
// 事故当时：脚本只看得到端口并集（required:["waoo"]），于是
// `--to script.echo --data '{"message":"hello"}'` 被判成「缺 waoo」→ exit 4 →
// 整条链 failed。现在每个候选都必须带自己的 needs，作者照着拼就不会撞墙。
func TestBuildEnvEmitPortsCandidatesCarryOwnNeeds(t *testing.T) {
	echoNeeds := `{"type":"object","properties":{"message":{"type":"string"}}}`
	waooNeeds := `{"type":"object","properties":{"waoo":{"type":"string"}},"required":["waoo"]}`
	j := job{
		TaskID: "tsk_author", Capability: "haha", Version: 1,
		Ports: []string{"out"},
		Expects: map[string]ws.PortExpect{
			"out": {
				// 中枢下发的并集：两个候选 required 的合并（不点名时的门槛）。
				Schema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"},"waoo":{"type":"string"}},"required":["waoo"]}`),
				Targets: []ws.ExpectTarget{
					{Capability: "script.echo", Version: 1, Needs: json.RawMessage(echoNeeds)},
					{Capability: "waoo", Version: 1, Needs: json.RawMessage(waooNeeds)},
				},
			},
		},
	}
	env, err := buildEnv(nil, j, t.TempDir(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	raw, _ := envLookup(env, envEmitPorts)

	// 一个脚本 / LLM 能做的两件事：① 知道「不点名只走一个」，② 按名字查到自己要什么。
	var contracts []emitPortContract
	if err := json.Unmarshal([]byte(raw), &contracts); err != nil {
		t.Fatalf("契约不是合法 JSON: %v (%s)", err, raw)
	}
	if len(contracts) != 1 || contracts[0].Port != "out" {
		t.Fatalf("contracts = %+v, want 单个 out", contracts)
	}
	if contracts[0].Pick != emitPickOne {
		t.Fatalf("pick = %q, want %q（同出口的多个下游是候选，不是扇出）", contracts[0].Pick, emitPickOne)
	}
	byName := map[string]emitTargetContract{}
	for _, tgt := range contracts[0].Targets {
		byName[tgt.Capability] = tgt
	}
	for name, want := range map[string]string{"script.echo": echoNeeds, "waoo": waooNeeds} {
		tgt, ok := byName[name]
		if !ok {
			t.Fatalf("targets 里没有 %s：%+v", name, contracts[0].Targets)
		}
		if string(tgt.Needs) != want {
			t.Fatalf("%s.needs = %s, want %s", name, tgt.Needs, want)
		}
	}

	// 反面对照：端口并集仍然要求 waoo —— 这正是当初骗到作者的那一条。两个字段并存
	// 不是重复，而是各答各的问题（不点名 vs 点名）。删掉 needs 就等于把作者又扔回并集。
	var union struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(contracts[0].Schema, &union); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if len(union.Required) != 1 || union.Required[0] != "waoo" {
		t.Fatalf("union.required = %v, want [waoo]（不点名时的门槛）", union.Required)
	}
	var echo struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(byName["script.echo"].Needs, &echo); err != nil {
		t.Fatalf("script.echo.needs: %v", err)
	}
	if len(echo.Required) != 0 {
		t.Fatalf("script.echo.needs.required = %v, want 空（它只要可选的 message）", echo.Required)
	}
}
