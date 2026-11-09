package hub

import (
	"fmt"
	"net/url"
	"strings"
)

// StreamPath 是节点 WS 端点（api-conventions.md §8.2：GET /v1/node/stream）。
//
// 父会话裁决：server 为空 path 时补这条**唯一**既定路径；显式 path 原样使用
// （反向代理可以把它挂到别处）。不接受 /v1/node/ws 之类的试拟路径。
const StreamPath = "/v1/node/stream"

// StreamURL 把配置里的 server（ws:// / wss://）解析成实际连接 URL。
func StreamURL(server string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil {
		return "", fmt.Errorf("hub: server 地址不合法 %q: %w", server, err)
	}
	switch u.Scheme {
	case "ws", "wss":
	default:
		return "", fmt.Errorf("hub: server 必须是 ws:// 或 wss://，实际 %q", server)
	}
	if u.Host == "" {
		return "", fmt.Errorf("hub: server 缺少主机名: %q", server)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = StreamPath
	}
	return u.String(), nil
}
