package protocol

import "testing"

// TestNodeStreamPath 钉节点 WS 端点：与 NodeBinaryPath 并列的共享常量。
//
// 中枢侧的空 path 或 "/" 归一为 NodeStreamPath，显式配置（反代前缀）原样使用；
// 节点侧只消费本常量，两侧不许各写一份字面量，也不提供 legacy 别名。
func TestNodeStreamPath(t *testing.T) {
	if NodeStreamPath != "/v1/node/stream" {
		t.Fatalf("NodeStreamPath = %q，期望 /v1/node/stream", NodeStreamPath)
	}
	if NodeBinaryPath != "/v1/node/binary" {
		t.Fatalf("NodeBinaryPath = %q，期望 /v1/node/binary", NodeBinaryPath)
	}
	if NodeStreamPath == NodeBinaryPath {
		t.Fatal("WS 端点与二进制下载端点不能是同一个路径")
	}
	for name, p := range map[string]string{"NodeStreamPath": NodeStreamPath, "NodeBinaryPath": NodeBinaryPath} {
		if p == "" || p[0] != '/' {
			t.Fatalf("%s = %q，必须是以 / 开头的根相对路径", name, p)
		}
	}
}
