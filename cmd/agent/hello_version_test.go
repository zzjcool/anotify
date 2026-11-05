package main

// 本文件钉住契约 §3 的**发送侧**：hello 帧里真的带 agent_version。
//
// 为什么单独立一个文件：这条链路有三段（internal/version → config.hello → 线上
// JSON），任何一段断开都**不会编译失败**——症状只是「控制台永远显示未知版本」。
// 所以断言落在**线上字节**上（走 serveNode 的真发送路径），而不是断言结构体字段
// 被赋过值。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/version"
)

// TestClientHelloCarriesAgentVersion：真握手的 hello 帧里，agent_version 必须是
// version.String() 的结果（注入的版本 + commit）。
func TestClientHelloCarriesAgentVersion(t *testing.T) {
	// 注入一个非 dev 的版本：否则「恰好等于缺省值」也能通过，测不出接线断没断。
	oldVersion, oldCommit := version.Version, version.Commit
	version.Version, version.Commit = "1.5.0", "3fd5a59"
	t.Cleanup(func() { version.Version, version.Commit = oldVersion, oldCommit })

	h := w4Start(t, w4Config(), 10, 100*time.Millisecond)
	defer h.stop()

	hello := h.fc.frames()[0]
	if hello["type"] != frameTypeHello {
		t.Fatalf("第一帧必须是 hello，实际 %#v", hello)
	}
	got, ok := hello["agent_version"].(string)
	if !ok {
		t.Fatalf("hello.agent_version 缺失或不是字符串（契约 §3：握手要自报版本），帧 = %#v", hello)
	}
	if want := "1.5.0+3fd5a59"; got != want {
		t.Errorf("hello.agent_version = %q, want %q（version.Version+Commit）", got, want)
	}
}

// TestHelloAgentVersionDevBuild：本地裸 build（没注入 -ldflags）要报 "dev"，
// **不能**是空串。
//
// 空串在中枢眼里等于「老版本 agent」——而 agent_version 带 omitempty，空值会让
// 这个键直接消失。两者在控制台上长得一样，但含义完全不同：一个是「这台机器的
// agent 太老，得升级」，一个是「这台机器跑的是开发构建」。
func TestHelloAgentVersionDevBuild(t *testing.T) {
	oldVersion, oldCommit := version.Version, version.Commit
	version.Version, version.Commit = "dev", ""
	t.Cleanup(func() { version.Version, version.Commit = oldVersion, oldCommit })

	cfg := mustLoadNodeConfig(t, nodeYAMLBootstrap)
	data, err := json.Marshal(cfg.hello(nil))
	if err != nil {
		t.Fatalf("json.Marshal(hello): %v", err)
	}
	if want := `"agent_version":"dev"`; !strings.Contains(string(data), want) {
		t.Errorf("hello = %s，want 含 %s（空值会被 omitempty 吞掉，等于冒充老版本）", data, want)
	}
}
