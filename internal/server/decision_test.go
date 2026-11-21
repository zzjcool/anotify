package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

type decisionAPIEnv struct {
	app    *App
	nsID   string
	taskID string
	token  string
}

func newDecisionAPIEnv(t *testing.T) *decisionAPIEnv {
	t.Helper()
	app := newProductionAppForTest(t)
	ctx := context.Background()
	db := app.DB
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	create := func(name string, next ...string) *store.CapabilityVersion {
		c := &store.CapabilityVersion{
			NamespaceID: ns.ID, Name: name, Version: 1,
			Spec: artifact.Spec{
				Next: append([]string{}, next...),
				Run:  artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\n"}},
			},
		}
		if cerr := db.CreateCapability(ctx, c); cerr != nil {
			t.Fatalf("建能力 %s: %v", name, cerr)
		}
		got, cerr := db.GetCapability(ctx, ns.ID, name, 1)
		if cerr != nil {
			t.Fatalf("回读能力 %s: %v", name, cerr)
		}
		return got
	}
	router := create("issue-router", "issue-close")
	create("issue-close")
	node := &store.Node{Name: "router-node", MaxInstances: 2, MaxConcurrency: 2,
		DeclaredNamespaces: []string{"default"}}
	nodeToken, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("建节点: %v", err)
	}
	stored, err := db.GetNodeByName(ctx, node.Name)
	if err != nil {
		t.Fatalf("回读节点: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, stored.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("节点上线: %v", err)
	}
	task := &store.Task{
		NamespaceID: ns.ID, TraceID: "trace-api-decision",
		CapabilityID: router.ID, Capability: router.Name,
		Version: router.Version, Digest: router.Digest,
		Params:   json.RawMessage(`{}`),
		Outcomes: json.RawMessage(`{"close":[{"capability":"issue-close"}],"drop":[]}`),
	}
	if err := db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, stored.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	if err := db.AssignTask(ctx, task.ID, stored.ID, "asg_api", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	_, _, decisionToken, _, _, err := db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: stored.ID, NodeTokenHash: store.HashNodeToken(nodeToken)},
		task.ID, "asg_api", 1, 0, store.Now())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return &decisionAPIEnv{app: app, nsID: ns.ID, taskID: task.ID, token: decisionToken}
}

func submitDecision(t *testing.T, env *decisionAPIEnv, taskID, bearer, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, protocol.DecisionPath(taskID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", protocol.DecisionContentType)
	if bearer != "" {
		req.Header.Set(protocol.DecisionAuthHeader, protocol.DecisionAuthScheme+" "+bearer)
	}
	rr := httptest.NewRecorder()
	env.app.Handler.ServeHTTP(rr, req)
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, rr.Body.String())
	}
	return rr.Code, doc
}

func TestDecisionEndpointAuthBeforeBody(t *testing.T) {
	env := newDecisionAPIEnv(t)
	cases := []struct {
		name   string
		taskID string
		bearer string
		body   string
	}{
		{"无凭据（body 合法）", env.taskID, "", `{"outcome":"close"}`},
		{"无凭据（body 也坏）", env.taskID, "", `{`},
		{"错凭据", env.taskID, "tok_not_mine", `{"outcome":"close"}`},
		{"任务不存在", "tsk_does_not_exist", env.token, `{"outcome":"close"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, doc := submitDecision(t, env, tc.taskID, tc.bearer, tc.body)
			if code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，want 401（body=%v）", code, doc)
			}
			if doc["reason"] != protocol.ReasonTokenInvalid {
				t.Fatalf("reason = %v，want %s", doc["reason"], protocol.ReasonTokenInvalid)
			}
		})
	}
}

func TestDecisionEndpointOverBoundThenIdempotent(t *testing.T) {
	env := newDecisionAPIEnv(t)

	code, doc := submitDecision(t, env, env.taskID, env.token, `{"outcome":"issue-close"}`)
	if code != http.StatusForbidden {
		t.Fatalf("越界状态码 = %d，want 403（body=%v）", code, doc)
	}
	if doc["reason"] != protocol.ReasonOutcomeNotDeclared {
		t.Fatalf("越界 reason = %v，want %s", doc["reason"], protocol.ReasonOutcomeNotDeclared)
	}

	code, doc = submitDecision(t, env, env.taskID, env.token, `{"outcome":"close","params":{"reason":"邮件纠纷"}}`)
	if code != http.StatusOK {
		t.Fatalf("判决状态码 = %d，want 200（body=%v）", code, doc)
	}
	if doc["outcome"] != "close" || doc["actions"] != float64(1) || doc["taskId"] != env.taskID {
		t.Fatalf("判决回执不对：%v", doc)
	}

	code, doc = submitDecision(t, env, env.taskID, env.token, `{"outcome":"drop"}`)
	if code != http.StatusConflict {
		t.Fatalf("重复判决状态码 = %d，want 409（body=%v）", code, doc)
	}
	if doc["reason"] != protocol.ReasonDecisionAlreadyTaken {
		t.Fatalf("重复判决 reason = %v，want %s", doc["reason"], protocol.ReasonDecisionAlreadyTaken)
	}
}

func TestDecisionEndpointMalformedBody(t *testing.T) {
	env := newDecisionAPIEnv(t)
	code, doc := submitDecision(t, env, env.taskID, env.token, `{`)
	if code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 状态码 = %d，want 400（body=%v）", code, doc)
	}
	code, doc = submitDecision(t, env, env.taskID, env.token, `{"outcome":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("空结局名状态码 = %d，want 400（body=%v）", code, doc)
	}
}
