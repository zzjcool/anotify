package server

// status.observed 的资源层门禁（CONTRACT-observed）：
//   - 有事实时 envelope 里四个键齐全、形状与 api/resource.yaml 的具名 schema 一致；
//   - 无事实时仍是**空对象 `{}`**（升级前的行为，客户端不需要改）。
//
// 事实用直插 SQL 造（任务 / emit / route 观察事件）：本文件验的是读面映射，
// 不是写路径；走 router + 节点租约只会把同一批 SELECT 埋进几百行准备代码里。

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// observedEnvExec 是直插 SQL 的小工具（失败即 Fatal）。
func observedEnvExec(t *testing.T, env *capTestEnv, query string, args ...any) {
	t.Helper()
	if _, err := env.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestCapabilityStatusObserved(t *testing.T) {
	env := newCapTestEnv(t)

	t.Run("无事实：observed 是空对象", func(t *testing.T) {
		env.createCapability(t, env.nsA, "never-ran", "#!/bin/sh\necho never\n")
		rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "never-ran", ""), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: status=%d body=%s", rec.Code, rec.Body.String())
		}
		observed := capObject(t, capObject(t, decodeCapJSON(t, rec), "信封")["status"], "status")["observed"]
		obj := capObject(t, observed, "status.observed")
		if len(obj) != 0 {
			t.Fatalf("从没跑过的能力，status.observed 必须是空对象 {}，实际 %#v", obj)
		}
	})

	t.Run("有事实：taskId/emits/pruned/tasks 齐全", func(t *testing.T) {
		env.createCapability(t, env.nsA, "observed-demo", "#!/bin/sh\necho demo\n")

		// 能力行的 UID 是任务行的外键（tasks_resolved_pin_shape 要求 digest/version 写实）。
		var capID, digest string
		if err := env.db.QueryRow(
			`SELECT id, digest FROM capability_versions WHERE namespace_id = ? AND name = ?`,
			env.nsA, "observed-demo").Scan(&capID, &digest); err != nil {
			t.Fatalf("read capability row: %v", err)
		}

		now := store.Now()
		taskID := store.NewTaskID()
		traceID := "trc_" + taskID
		// 最近任务 + 两条同能力的窗口内任务（一条 failed）→ tasks{total:3, failed:1}。
		observedEnvExec(t, env, `INSERT INTO tasks
			(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at)
			VALUES (?, ?, ?, ?, 'observed-demo', 1, ?, '{}', ?, ?)`,
			taskID, env.nsA, traceID, capID, digest, store.TaskSucceeded, now)
		for i, phase := range []string{store.TaskFailed, store.TaskSucceeded} {
			observedEnvExec(t, env, `INSERT INTO tasks
				(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at)
				VALUES (?, ?, ?, ?, 'observed-demo', 1, ?, '{}', ?, ?)`,
				store.NewTaskID(), env.nsA, fmt.Sprintf("trc_older_%d", i), capID, digest, phase, now-int64(60*(i+1)))
		}

		// 两条 emit：一条真派发（ok=true），一条剪枝回执（ok=false）。
		observedEnvExec(t, env, `INSERT INTO emits (id, task_id, to_ref, data, result, created_at)
			VALUES (?, ?, 'analyze-pr', '{"repo":"anotify","diff":"a.patch"}',
			        '{"taskId":"tsk_child","eventId":"emt_1","to":"analyze-pr","version":2}', ?)`,
			store.NewEventID(), taskID, now+1)
		observedEnvExec(t, env, `INSERT INTO emits (id, task_id, to_ref, data, result, created_at)
			VALUES (?, ?, 'lint-fix', '{}',
			        '{"taskId":null,"eventId":"emt_2","to":"lint-fix","pruned":{"reason":"param_missing","detail":"missing required param: diff"}}', ?)`,
			store.NewEventID(), taskID, now+2)

		// 一条属于最近任务的剪枝观察事件（source=route、type=剪枝码、payload.taskId 指向它）。
		observedEnvExec(t, env, `INSERT INTO events
			(id, namespace_id, trace_id, source, type, payload, reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			store.NewEventID(), env.nsA, traceID, store.EventPruneSource, protocol.ReasonParamMissing,
			`{"capability":"lint-fix","reason":"param_missing","detail":"missing required param: diff","taskId":"`+taskID+`","parentTaskId":"`+taskID+`"}`,
			store.EventReasonPruned, now+3)

		rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "observed-demo", ""), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: status=%d body=%s", rec.Code, rec.Body.String())
		}
		doc := decodeCapJSON(t, rec)
		observed := capObject(t, capObject(t, doc, "信封")["status"], "status")["observed"]
		obj := capObject(t, observed, "status.observed")

		if got := capString(t, obj, "taskId"); got != taskID {
			t.Fatalf("observed.taskId = %q, want %q", got, taskID)
		}

		emits := capArray(t, obj["emits"], "observed.emits")
		if len(emits) != 2 {
			t.Fatalf("observed.emits 有 %d 条，want 2: %#v", len(emits), emits)
		}
		first := capObject(t, emits[0], "emits[0]")
		if got := capString(t, first, "to"); got != "analyze-pr" {
			t.Fatalf("emits[0].to = %q", got)
		}
		if got := capBool(t, first, "ok"); !got {
			t.Fatalf("emits[0].ok = false，want true（回执点名了真实子任务）")
		}
		fields := capArray(t, first["fields"], "emits[0].fields")
		if len(fields) != 2 || fields[0] != "diff" || fields[1] != "repo" {
			t.Fatalf("emits[0].fields = %#v，want [diff repo]（键名排序后）", fields)
		}
		second := capObject(t, emits[1], "emits[1]")
		if got := capBool(t, second, "ok"); got {
			t.Fatalf("emits[1].ok = true，want false（剪枝回执 = 收到但没建任务）")
		}
		if got := capArray(t, second["fields"], "emits[1].fields"); len(got) != 0 {
			t.Fatalf("emits[1].fields = %#v，want 空数组（请求体 {} 没有键名）", got)
		}

		pruned := capArray(t, obj["pruned"], "observed.pruned")
		if len(pruned) != 1 {
			t.Fatalf("observed.pruned 有 %d 条，want 1: %#v", len(pruned), pruned)
		}
		item := capObject(t, pruned[0], "pruned[0]")
		if got := capString(t, item, "to"); got != "lint-fix" {
			t.Fatalf("pruned[0].to = %q, want lint-fix", got)
		}
		if got := capString(t, item, "reason"); got != protocol.ReasonParamMissing {
			t.Fatalf("pruned[0].reason = %q, want %q", got, protocol.ReasonParamMissing)
		}
		if got := capString(t, item, "detail"); got != "missing required param: diff" {
			t.Fatalf("pruned[0].detail = %q", got)
		}

		tasks := capObject(t, obj["tasks"], "observed.tasks")
		if got := capNumber(t, tasks, "total"); got != 3 {
			t.Fatalf("observed.tasks.total = %d, want 3", got)
		}
		if got := capNumber(t, tasks, "failed"); got != 1 {
			t.Fatalf("observed.tasks.failed = %d, want 1", got)
		}
	})
}

// TestCapabilityStatusObservedDegrades 守「读面绝不因为 observed 查询失败而 500」。
//
// 造失败的办法是真的：把 events 表改名——statusFor 的其它派生面不看这张表，
// 所以只有 observed 的那条 SELECT 会错。断言 GET 仍然 200 且 observed 是 {}。
func TestCapabilityStatusObservedDegrades(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "degrade-demo", "#!/bin/sh\necho degrade\n")

	var capID, digest string
	if err := env.db.QueryRow(
		`SELECT id, digest FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		env.nsA, "degrade-demo").Scan(&capID, &digest); err != nil {
		t.Fatalf("read capability row: %v", err)
	}
	observedEnvExec(t, env, `INSERT INTO tasks
		(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at)
		VALUES (?, ?, ?, ?, 'degrade-demo', 1, ?, '{}', ?, ?)`,
		store.NewTaskID(), env.nsA, "trc_degrade", capID, digest, store.TaskSucceeded, store.Now())

	observedEnvExec(t, env, `ALTER TABLE events RENAME TO events_moved`)

	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "degrade-demo", ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("observed 查询失败时 GET 必须仍 200（降级成空对象），实际 status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	obj := capObject(t,
		capObject(t, capObject(t, decodeCapJSON(t, rec), "信封")["status"], "status")["observed"],
		"status.observed")
	if len(obj) != 0 {
		t.Fatalf("降级后 status.observed 必须是 {}，实际 %#v", obj)
	}
}

// capBool 读一个布尔字段（缺键/类型不符都算测试失败）。
func capBool(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("%s 不是布尔: %#v", key, m[key])
	}
	return v
}
