package server

// resource_node_config_notify_test.go 是 #569 的 server 层回归：配置写入口成功后必须推 config.changed。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

type nodeConfigNotifyFixture struct {
	db     *store.DB
	mux    *http.ServeMux
	cookie *http.Cookie
	node   *store.Node
	token  string
	hub    *ws.Hub
	srv    *hubServer
	deps   *serverDeps
	logs   *bytes.Buffer
}

func newNodeConfigNotifyFixture(t *testing.T) *nodeConfigNotifyFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new auth service: %v", err)
	}
	uid, err := db.InsertUser(ctx, "", "cfg-notify-"+store.NewUserID(), "Config Notify")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Config notify · test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	node := &store.Node{Name: "cfg-node-" + store.NewUserID()}
	token, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	hub := ws.NewHub(db, route.New(db))
	logs := &bytes.Buffer{}
	deps := &serverDeps{
		DB: db, Router: route.New(db), Hub: hub,
		Log: slog.New(slog.NewTextHandler(logs, nil)), Now: nowMilli, Session: svc.Sessions().Middleware,
	}
	mux := http.NewServeMux()
	newNodeAPI(deps).register(mux)

	f := &nodeConfigNotifyFixture{
		db: db, mux: mux,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		node:   node, token: token, hub: hub, deps: deps, logs: logs,
	}
	f.srv = newHubServer(t, hub)
	return f
}

func (f *nodeConfigNotifyFixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.AddCookie(f.cookie)
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

func (f *nodeConfigNotifyFixture) connect(t *testing.T) *websocket.Conn {
	t.Helper()
	conn := f.srv.dial(t)
	slot := 0
	writeNodeFrame(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: f.node.Name, Token: f.token,
		Namespaces: []string{"default"},
		Instances:  []protocol.HelloInstance{{Slot: &slot}},
	})
	frame, err := upgradeReadFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读 welcome: %v", err)
	}
	welcome, ok := frame.(*protocol.WelcomeFrame)
	if !ok {
		t.Fatalf("hello 之后应收到 welcome，得到 %T", frame)
	}
	hash, err := protocol.MachineConfigHash(welcome.NodeVars, welcome.NodeLimits)
	if err != nil {
		t.Fatalf("算机器配置 hash: %v", err)
	}
	writeNodeFrame(t, conn, &protocol.ConfigAppliedFrame{
		Type: protocol.FrameConfigApplied, Revision: welcome.ConfigRevision, Hash: hash,
	})
	// 有序屏障：serveSession 在 attach 后逐帧串行，config.get 的 welcome 回来即证明 applied 已落库。
	writeNodeFrame(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	frame, err = upgradeReadFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读屏障 welcome: %v", err)
	}
	if _, ok := frame.(*protocol.WelcomeFrame); !ok {
		t.Fatalf("config.get 的屏障应答应是 welcome，得到 %T", frame)
	}
	return conn
}

func writeNodeFrame(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	data, err := protocol.Encode(frame)
	if err != nil {
		t.Fatalf("encode frame %T: %v", frame, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame %T: %v", frame, err)
	}
}

func expectConfigChanged(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	frame, err := upgradeReadFrame(t, conn, time.Second)
	if err != nil {
		t.Fatalf("读帧: %v", err)
	}
	if frame == nil {
		t.Fatalf("配置写入后 1s 内没有收到 config.changed（中枢没推：在线节点会永远停在旧 revision）")
	}
	changed, ok := frame.(*protocol.ConfigChangedFrame)
	if !ok {
		t.Fatalf("配置写入后第一帧应是 config.changed，得到 %T", frame)
	}
	if changed.Revision != want {
		t.Fatalf("config.changed.revision = %q, want %q", changed.Revision, want)
	}
}

func (f *nodeConfigNotifyFixture) configRV(t *testing.T) string {
	t.Helper()
	rr := f.do(t, http.MethodGet, "/v1/nodes/"+f.node.Name+"/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET config status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var out struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return out.Metadata.ResourceVersion
}

func (f *nodeConfigNotifyFixture) configRevision(t *testing.T) string {
	t.Helper()
	cfg, err := f.db.GetNodeConfig(context.Background(), f.node.ID)
	if err != nil {
		t.Fatalf("read node config: %v", err)
	}
	return strconv.FormatInt(cfg.Revision, 10)
}

func TestNodeConfigWritePushesConfigChanged(t *testing.T) {
	t.Run("PUT /v1/nodes/{name}/config", func(t *testing.T) {
		f := newNodeConfigNotifyFixture(t)
		conn := f.connect(t)

		rr := f.do(t, http.MethodPut, "/v1/nodes/"+f.node.Name+"/config", map[string]any{
			"metadata":   map[string]any{"resourceVersion": f.configRV(t)},
			"namespaces": []string{"default"},
			"vars":       map[string]string{"FOO": "bar"},
			"limits":     map[string]any{"total": 3},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		revision, _ := body["configRevision"].(string)
		if revision == "" || revision == "0" {
			t.Fatalf("configRevision 应前进，得到 %v（body=%s）", body["configRevision"], rr.Body.String())
		}
		if got, _ := body["appliedRevision"].(string); got == revision {
			t.Fatalf("写入不该把 appliedRevision 谎报成 %q（body=%s)", revision, rr.Body.String())
		}
		expectConfigChanged(t, conn, revision)
	})

	t.Run("POST /v1/nodes/config/batch", func(t *testing.T) {
		f := newNodeConfigNotifyFixture(t)
		conn := f.connect(t)

		rr := f.do(t, http.MethodPost, "/v1/nodes/config/batch", map[string]any{
			"updates": []map[string]any{{
				"name":     f.node.Name,
				"metadata": map[string]any{"resourceVersion": f.configRV(t)},
				"vars":     map[string]string{"REGION": "cn-east"},
			}},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		var out struct {
			Items []struct {
				Name   string         `json:"name"`
				OK     bool           `json:"ok"`
				Config map[string]any `json:"config"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode batch: %v", err)
		}
		if len(out.Items) != 1 || !out.Items[0].OK {
			t.Fatalf("批量应逐台成功：%s", rr.Body.String())
		}
		expectConfigChanged(t, conn, f.configRevision(t))
	})

	t.Run("PUT /v1/nodes/{name}/yaml", func(t *testing.T) {
		f := newNodeConfigNotifyFixture(t)
		conn := f.connect(t)

		doc := fmt.Sprintf(`apiVersion: anotify/v1
kind: Node
metadata:
  name: %s
  resourceVersion: %q
spec:
  namespaces:
    - default
  vars:
    FOO: bar
`, f.node.Name, f.configRV(t))
		rr := f.do(t, http.MethodPut, "/v1/nodes/"+f.node.Name+"/yaml", map[string]any{"yaml": doc})
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT yaml status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		expectConfigChanged(t, conn, f.configRevision(t))
	})

	t.Run("PATCH /v1/nodes/{name} 带 vars", func(t *testing.T) {
		f := newNodeConfigNotifyFixture(t)
		conn := f.connect(t)

		rr := f.do(t, http.MethodPatch, "/v1/nodes/"+f.node.Name, map[string]any{
			"metadata": map[string]any{"resourceVersion": f.configRV(t)},
			"spec":     map[string]any{"vars": map[string]string{"FOO": "bar"}},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		expectConfigChanged(t, conn, f.configRevision(t))
	})

	t.Run("PATCH 只改 tags 不 bump revision → 不发帧", func(t *testing.T) {
		f := newNodeConfigNotifyFixture(t)
		conn := f.connect(t)
		before := f.configRevision(t)

		rr := f.do(t, http.MethodPatch, "/v1/nodes/"+f.node.Name, map[string]any{
			"metadata": map[string]any{"resourceVersion": f.configRV(t)},
			"spec":     map[string]any{"tags": []string{"gpu"}},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
		}
		if after := f.configRevision(t); after != before {
			t.Fatalf("只改 tags 不该 bump config revision（%s → %s）", before, after)
		}
		frame, err := upgradeReadFrame(t, conn, 300*time.Millisecond)
		if err != nil {
			t.Fatalf("读帧: %v", err)
		}
		if frame != nil {
			t.Fatalf("已对齐的节点在「只改 tags」的 PATCH 后不该收到帧，得到 %T", frame)
		}
	})
}

// TestNodeConfigWriteSurvivesNotifyFailure：写库已成功，推送失败只记 WARN（#569 review r1）。
func TestNodeConfigWriteSurvivesNotifyFailure(t *testing.T) {
	f := newNodeConfigNotifyFixture(t)
	dead, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open dead db: %v", err)
	}
	if err := dead.Close(); err != nil {
		t.Fatalf("close dead db: %v", err)
	}
	f.deps.Hub = ws.NewHub(dead, route.New(f.db))

	rr := f.do(t, http.MethodPut, "/v1/nodes/"+f.node.Name+"/config", map[string]any{
		"metadata":   map[string]any{"resourceVersion": f.configRV(t)},
		"namespaces": []string{"default"},
		"vars":       map[string]string{"FOO": "bar"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("推送失败不该影响已经落库的写入：status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	if got := f.configRevision(t); got == "0" {
		t.Fatalf("配置必须真的落库，configRevision 仍是 %s", got)
	}
	if !strings.Contains(f.logs.String(), "api.nodes.config_notify_failed") {
		t.Fatalf("推送失败必须留下 WARN（event=api.nodes.config_notify_failed），日志：%s", f.logs.String())
	}
}
