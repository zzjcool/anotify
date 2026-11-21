package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

type decisionEnv struct {
	db      *DB
	nsID    string
	router  *CapabilityVersion
	closeC  *CapabilityVersion
	notifyC *CapabilityVersion
	node    *Node
	task    *Task
	token   string
	leaseID string
	attempt int
}

func newDecisionEnv(t *testing.T, outcomes string) *decisionEnv {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	router := v4Cap(t, db, ns.ID, "issue-router", 1, "issue-close", "issue-notify")
	closeC := v4Cap(t, db, ns.ID, "issue-close", 1)
	notifyC := v4Cap(t, db, ns.ID, "issue-notify", 1)
	node := v4Node(t, db, "router-node", 2)
	task := &Task{
		NamespaceID: ns.ID, TraceID: "trace-decision",
		CapabilityID: router.ID, Capability: router.Name,
		Version: router.Version, Digest: router.Digest,
		Params:   json.RawMessage(`{"event":{"issue":{"title":"spam"}}}`),
		Outcomes: json.RawMessage(outcomes),
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("建 router 任务: %v", err)
	}
	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-dec", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	leaseID, _, decisionToken, attempt, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, "asg-dec", 1, slot, Now())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if leaseID == "" || attempt != 1 {
		t.Fatalf("grant 事实不全: lease=%q attempt=%d", leaseID, attempt)
	}
	return &decisionEnv{db: db, nsID: ns.ID, router: router, closeC: closeC, notifyC: notifyC, node: node, task: task, token: decisionToken,
		leaseID: leaseID, attempt: attempt}
}

const decisionOutcomes = `{"close":[{"capability":"issue-close","params":{"reason":"$(decision.reason)"}},` +
	`{"capability":"issue-notify","params":{"channel":"#mod"}}],"drop":[]}`

func TestDecisionTokenOnlyForRouterTasks(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	if env.token == "" {
		t.Fatal("router 任务必须拿到决策凭据")
	}
	plain := v4Task(t, env.db, env.nsID, env.closeC, "trace-plain", "{}")
	if err := env.db.AssignTask(context.Background(), plain.ID, env.node.ID, "asg-plain", 1); err != nil {
		t.Fatalf("assign plain: %v", err)
	}
	_, _, token, _, _, err := env.db.GrantLeaseAuthorized(context.Background(),
		v4Auth(t, env.db, env.node.ID), plain.ID, "asg-plain", 1, v4FreeSlot(t, env.db, env.node.ID), Now())
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}
	if token != "" {
		t.Fatalf("没有 outcomes 的任务不该拿到决策凭据：%q", token)
	}
}

func TestTaskOutcomesRoundTrip(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	got, err := env.db.GetTaskByID(context.Background(), env.task.ID)
	if err != nil {
		t.Fatalf("回读任务: %v", err)
	}
	if string(got.Outcomes) != decisionOutcomes {
		t.Fatalf("outcomes 往返不一致：\n got %s\nwant %s", got.Outcomes, decisionOutcomes)
	}
	if len(got.Decision) != 0 {
		t.Fatalf("未判决的任务 decision 必须是空：%s", got.Decision)
	}
	if got.DecisionTokenHash == "" || got.DecisionExpiresAt == nil {
		t.Fatalf("凭据摘要/TTL 必须落库：hash=%q exp=%v", got.DecisionTokenHash, got.DecisionExpiresAt)
	}
	if _, err := DecodeOutcomeForTest(got.Outcomes); err != nil {
		t.Fatalf("outcomes 形状: %v", err)
	}
}

func DecodeOutcomeForTest(raw json.RawMessage) (map[string][]sourcespec.OutcomeAction, error) {
	var out map[string][]sourcespec.OutcomeAction
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func TestRecordDecisionHappyThenIdempotent(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	record, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "close",
		Params: map[string]any{"reason": "广告"},
	})
	if err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	if record.Actions != 2 {
		t.Fatalf("close 的动作数 = %d，want 2", record.Actions)
	}
	task, err := env.db.GetTaskByID(ctx, env.task.ID)
	if err != nil {
		t.Fatalf("回读任务: %v", err)
	}
	decision := ParseDecision(task.Decision)
	if decision == nil || decision.Outcome != "close" || decision.Params["reason"] != "广告" {
		t.Fatalf("判决事实落库不对：%s", task.Decision)
	}
	if task.DecisionAt == nil {
		t.Fatal("decision_at 必须落库（trace 诊断）")
	}

	if _, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "drop",
	}); !errors.Is(err, ErrDecisionAlreadyTaken) {
		t.Fatalf("第二次判决应 ErrDecisionAlreadyTaken，得到 %v", err)
	}
	task, _ = env.db.GetTaskByID(ctx, env.task.ID)
	if ParseDecision(task.Decision).Outcome != "close" {
		t.Fatalf("第二次提交改写了判决：%s", task.Decision)
	}
}

func TestRecordDecisionRejectsBadCredential(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	cases := []struct {
		name  string
		token string
		task  string
	}{
		{"错凭据", "tok_deadbeef", env.task.ID},
		{"空凭据", " ", env.task.ID}, // 形状错（400）：handler 在缺 Bearer 时更早就回 401
		{"不存在任务", env.token, "tsk_missing"},
		{"别任务的任务 id", env.token, "tsk_other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.db.RecordDecision(ctx, DecisionInput{TaskID: tc.task, Token: tc.token, Outcome: "close"})
			want := ErrAttemptTokenInvalid
			if tc.name == "空凭据" {
				want = ErrDecisionInvalid
			}
			if !errors.Is(err, want) {
				t.Fatalf("应 %v，得到 %v", want, err)
			}
		})
	}
	if _, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "close", Now: Now() + int64(protocol.DecisionTokenTTL.Seconds()) + 5,
	}); !errors.Is(err, ErrAttemptTokenInvalid) {
		t.Fatalf("过期凭据应 ErrAttemptTokenInvalid，得到 %v", err)
	}
}

func TestRecordDecisionOutcomeNotDeclaredWritesAuditRow(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	_, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "issue-close", // 动作名冒充结局名
	})
	if !errors.Is(err, ErrOutcomeNotDeclared) {
		t.Fatalf("越界结局应 ErrOutcomeNotDeclared，得到 %v", err)
	}
	var payload string
	if qerr := env.db.QueryRow(
		`SELECT payload FROM events WHERE namespace_id = ? AND trace_id = ? AND type = ? ORDER BY created_at DESC LIMIT 1`,
		env.nsID, env.task.TraceID, protocol.ReasonOutcomeNotDeclared).Scan(&payload); qerr != nil {
		t.Fatalf("越界必须留审计行: %v", qerr)
	}
	var audit map[string]any
	if jerr := json.Unmarshal([]byte(payload), &audit); jerr != nil {
		t.Fatalf("审计行 payload 不是 JSON: %v", jerr)
	}
	if audit["outcome"] != "issue-close" || audit["taskId"] != env.task.ID {
		t.Fatalf("审计行缺事实：%v", audit)
	}
	if declared, _ := audit["declared"].([]any); len(declared) != 2 {
		t.Fatalf("审计行应带声明表（close/drop）：%v", audit["declared"])
	}
	task, _ := env.db.GetTaskByID(ctx, env.task.ID)
	if len(task.Decision) != 0 {
		t.Fatalf("越界不能落判决：%s", task.Decision)
	}
	if n := countTraceTasks(t, env.db, env.nsID, env.task.TraceID); n != 1 {
		t.Fatalf("越界不能建任务（trace 里只有 router 任务本身）：%d", n)
	}
}

func TestRecordDecisionAfterTerminalIsConflict(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	if _, err := env.db.Exec(`UPDATE tasks SET phase = ?, finished_at = ? WHERE id = ?`,
		TaskSucceeded, Now(), env.task.ID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	if _, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "close",
	}); !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("终态任务的判决应 ErrTaskNotRunning，得到 %v", err)
	}
}

func TestSourceTargetsRejectOutcomesOutsideNext(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	v4Cap(t, db, ns.ID, "issue-router", 1, "issue-close")
	v4Cap(t, db, ns.ID, "issue-close", 1)
	source := &Source{
		NamespaceID: ns.ID, Name: "gh", Kind: "webhook", Enabled: true,
		Config: json.RawMessage(`{}`),
		Targets: []sourcespec.Target{{
			Capability: "issue-router",
			Outcomes: map[string][]sourcespec.OutcomeAction{
				"close":  {{Capability: "issue-close"}},
				"triage": {{Capability: "issue-notify"}}, // 不在 next 里
			},
		}},
	}
	err = db.CreateSource(ctx, source)
	var causes *sourcespec.CausesError
	if !errors.As(err, &causes) {
		t.Fatalf("应返回 CausesError（400 + details.causes），得到 %v", err)
	}
	if len(causes.Causes) != 1 {
		t.Fatalf("应只有一条 cause：%v", causes.Causes)
	}
	if got, want := causes.Causes[0].Field, "targets[0].outcomes.triage[0].capability"; got != want {
		t.Fatalf("cause.field = %q，want %q", got, want)
	}
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("必须仍能 errors.Is(ErrInvalidSourceConfig)：%v", err)
	}
	source.Name = "gh-ok"
	source.Targets[0].Outcomes["triage"] = []sourcespec.OutcomeAction{{Capability: "issue-close"}}
	if err := db.CreateSource(ctx, source); err != nil {
		t.Fatalf("合法的 outcomes 应能建源：%v", err)
	}
	got, err := db.GetSourceByName(ctx, ns.ID, "gh-ok")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	if len(got.Targets[0].Outcomes["triage"]) != 1 || got.Targets[0].Outcomes["triage"][0].Capability != "issue-close" {
		t.Fatalf("outcomes 往返丢了：%+v", got.Targets[0].Outcomes)
	}
}

func TestCreateOutcomeDispatchWritesTasksFailuresAndPrunes(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	child := &Task{
		NamespaceID: env.nsID, TraceID: env.task.TraceID,
		CapabilityID: env.closeC.ID, Capability: env.closeC.Name,
		Version: env.closeC.Version, Digest: env.closeC.Digest,
		Params: json.RawMessage(`{"reason":"x"}`),
	}
	err := env.db.CreateOutcomeDispatch(ctx, OutcomeDispatchInputs{
		NamespaceID: env.nsID, TraceID: env.task.TraceID, ParentTaskID: env.task.ID, Now: Now(),
		Tasks:    []*Task{child},
		Failures: []DispatchFailureInputs{{Capability: "ghost", Detail: "下游解析不到"}},
		Prunes:   []PruneInputs{{Capability: "issue-close", Reason: protocol.ReasonParamMissing, Detail: "需要 reason"}},
	})
	if err != nil {
		t.Fatalf("派发事实应一次写成: %v", err)
	}
	got, err := env.db.GetTaskByID(ctx, child.ID)
	if err != nil {
		t.Fatalf("回读子任务: %v", err)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != env.task.ID {
		t.Fatalf("子任务必须挂到 router 任务上：%v", got.ParentTaskID)
	}
	if n := countTraceTasks(t, env.db, env.nsID, env.task.TraceID); n != 3 {
		t.Fatalf("trace 里应有 router + 子任务 + 失败任务 = 3，得到 %d", n)
	}
	if err := env.db.CreateOutcomeDispatch(ctx, OutcomeDispatchInputs{
		NamespaceID: env.nsID, TraceID: env.task.TraceID, Now: Now(),
	}); err != nil {
		t.Fatalf("空派发应 no-op: %v", err)
	}
	if err := env.db.CreateOutcomeDispatch(ctx, OutcomeDispatchInputs{
		NamespaceID: env.nsID, TraceID: env.task.TraceID, Now: Now(),
		Prunes: []PruneInputs{{Reason: protocol.ReasonDownstreamMissing}},
	}); err == nil {
		t.Fatal("downstream_missing 走失败 Task，不能当剪枝写")
	}
}

func TestInsertRouteObservationVocabulary(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	for _, reason := range protocol.RouteObservationReasons() {
		if err := env.db.InsertRouteObservation(ctx, env.nsID, env.task.TraceID, reason,
			map[string]any{"taskId": env.task.ID}); err != nil {
			t.Fatalf("观察码 %q 应能写: %v", reason, err)
		}
	}
	if err := env.db.InsertRouteObservation(ctx, env.nsID, env.task.TraceID, "bogus_code", nil); err == nil {
		t.Fatal("未登记的观察码必须被拒（封闭词表）")
	}
	var n int
	if err := env.db.QueryRow(`SELECT count(*) FROM events WHERE source = ? AND trace_id = ?`,
		EventPruneSource, env.task.TraceID).Scan(&n); err != nil {
		t.Fatalf("count route events: %v", err)
	}
	if n != len(protocol.RouteObservationReasons()) {
		t.Fatalf("route 观察行 = %d，want %d", n, len(protocol.RouteObservationReasons()))
	}
}

func countTraceTasks(t *testing.T, db *DB, nsID, traceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE namespace_id = ? AND trace_id = ?`,
		nsID, traceID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// TestDecisionTokenExpiryFollowsLeaseRenew 钉住「凭据有效期跟着租约续」（#522）：只续租约会让跑超过 10 分钟的 router 被判 401 → no_decision。
func TestDecisionTokenExpiryFollowsLeaseRenew(t *testing.T) {
	env := newDecisionEnv(t, decisionOutcomes)
	ctx := context.Background()
	before := *env.decisionExpiry(t)
	renewAt := before - int64(protocol.DecisionTokenTTL.Seconds()) + int64(protocol.LeaseTTL.Seconds())/2
	if _, err := env.db.RenewLeaseAuthorized(ctx, v4Auth(t, env.db, env.node.ID),
		env.task.ID, env.attempt, env.leaseID, renewAt); err != nil {
		t.Fatalf("续租: %v", err)
	}
	if want, got := renewAt+int64(protocol.DecisionTokenTTL.Seconds()), *env.decisionExpiry(t); got != want {
		t.Fatalf("续租后凭据有效期 = %d，want %d（grant 时 %d）", got, want, before)
	}
	if _, err := env.db.RecordDecision(ctx, DecisionInput{
		TaskID: env.task.ID, Token: env.token, Outcome: "close", Now: before + 5,
	}); err != nil {
		t.Fatalf("续租后凭据应仍可用: %v", err)
	}

	env2 := newDecisionEnv(t, decisionOutcomes)
	if _, err := env2.db.RecordDecision(ctx, DecisionInput{
		TaskID: env2.task.ID, Token: env2.token, Outcome: "drop",
	}); err != nil {
		t.Fatalf("先判决: %v", err)
	}
	used := *env2.decisionExpiry(t)
	if _, err := env2.db.RenewLeaseAuthorized(ctx, v4Auth(t, env2.db, env2.node.ID),
		env2.task.ID, env2.attempt, env2.leaseID, used-int64(protocol.DecisionTokenTTL.Seconds())+5); err != nil {
		t.Fatalf("判决后续租: %v", err)
	}
	if got := *env2.decisionExpiry(t); got != used {
		t.Fatalf("已消费的凭据被续命：%d → %d", used, got)
	}
}

func (e *decisionEnv) decisionExpiry(t *testing.T) *int64 {
	t.Helper()
	task, err := e.db.GetTaskByID(context.Background(), e.task.ID)
	if err != nil {
		t.Fatalf("回读任务: %v", err)
	}
	if task.DecisionExpiresAt == nil {
		t.Fatal("决策凭据 TTL 未落库")
	}
	return task.DecisionExpiresAt
}
