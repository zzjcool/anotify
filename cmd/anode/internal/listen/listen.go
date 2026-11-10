// Package listen 解析并打开 anode 的唯一监听（emit 端点，anode.md §4）。
//
// 只有两种合法形态：`tcp://<host>:<port>`（默认 `tcp://127.0.0.1:0`）与
// `unix://<绝对路径>`。默认 loopback + 内核挑端口：emit 令牌是明文，默认不开口子。
//
// unix 形态是「安全不删任意已有路径」的：只有路径**确实是一个无人监听的
// unix socket**（连接被拒）才当作残留清掉；普通文件、目录、符号链接一律拒绝并
// 保持原样。关闭时只删除「我们自己创建的那个 inode」，路径被换掉就不动它。
package listen

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultSpec 是缺省监听：仅本机可连，端口由内核分配（anode.md §4）。
const DefaultSpec = "tcp://127.0.0.1:0"

// unixProbeTimeout 是判断「残留 socket 还是有人在听」的连接超时。
const unixProbeTimeout = 200 * time.Millisecond

// Spec 是解析后的监听地址。
type Spec struct {
	// Network 是 "tcp" 或 "unix"。
	Network string
	// Address 是 net.Listen 的地址（tcp 是 host:port，unix 是绝对路径）。
	Address string
}

// Parse 校验并归一一个 listen 值（空串 → 默认 tcp://127.0.0.1:0）。
//
// 其它 scheme（http:// 之类）返回 listen_invalid（anode.md §5）。
func Parse(spec string) (Spec, error) {
	if strings.TrimSpace(spec) == "" {
		spec = DefaultSpec
	}
	u, err := url.Parse(spec)
	if err != nil {
		return Spec{}, fmt.Errorf("listen_invalid: %q：%v", spec, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Spec{}, fmt.Errorf("listen_invalid: %q：不接受 query/fragment", spec)
	}
	switch u.Scheme {
	case "tcp":
		if u.Host == "" {
			return Spec{}, fmt.Errorf("listen_invalid: %q：tcp 需要显式 host（默认 127.0.0.1）", spec)
		}
		if u.Path != "" && u.Path != "/" {
			return Spec{}, fmt.Errorf("listen_invalid: %q：tcp 不接受路径", spec)
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			return Spec{}, fmt.Errorf("listen_invalid: %q：%v", spec, err)
		}
		if host == "" {
			return Spec{}, fmt.Errorf("listen_invalid: %q：tcp 需要显式 host（默认 127.0.0.1）", spec)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return Spec{}, fmt.Errorf("listen_invalid: %q：端口必须是 0..65535", spec)
		}
		return Spec{Network: "tcp", Address: net.JoinHostPort(host, strconv.Itoa(n))}, nil
	case "unix":
		// unix:///run/anode.sock：Host 必须为空，Path 是绝对路径。
		if u.Host != "" {
			return Spec{}, fmt.Errorf("listen_invalid: %q：unix 形态是 unix://<绝对路径>", spec)
		}
		p := u.Path
		if p == "" || !filepath.IsAbs(p) {
			return Spec{}, fmt.Errorf("listen_invalid: %q：unix 路径必须是绝对路径", spec)
		}
		if strings.ContainsRune(p, 0) {
			return Spec{}, fmt.Errorf("listen_invalid: %q：路径不能含 NUL", spec)
		}
		if filepath.Clean(p) != p {
			return Spec{}, fmt.Errorf("listen_invalid: %q：unix 路径必须是规范形式（不要 .. 或重复斜杠）", spec)
		}
		return Spec{Network: "unix", Address: p}, nil
	default:
		return Spec{}, fmt.Errorf("listen_invalid: %q：只支持 tcp:// / unix://", spec)
	}
}

// Listener 是打开的监听（emit 网关用）。
type Listener struct {
	net.Listener

	spec     Spec
	unixPath string
	unixInfo os.FileInfo
}

// Open 解析 spec 并真的开始监听；返回的 Listener 带**实际**端点。
func Open(spec string) (*Listener, error) {
	s, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	switch s.Network {
	case "tcp":
		ln, err := net.Listen("tcp", s.Address)
		if err != nil {
			return nil, fmt.Errorf("listen: tcp %s: %w", s.Address, err)
		}
		return &Listener{Listener: ln, spec: s}, nil
	case "unix":
		if err := prepareUnixSocket(s.Address); err != nil {
			return nil, err
		}
		ln, err := net.Listen("unix", s.Address)
		if err != nil {
			return nil, fmt.Errorf("listen: unix %s: %w", s.Address, err)
		}
		// 只有同用户能连：socket 本身 0600。
		_ = os.Chmod(s.Address, 0o600)
		info, err := os.Lstat(s.Address)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("listen: unix %s: %w", s.Address, err)
		}
		return &Listener{Listener: ln, spec: s, unixPath: s.Address, unixInfo: info}, nil
	default:
		return nil, fmt.Errorf("listen_invalid: %q", spec)
	}
}

// prepareUnixSocket 处理「路径已存在」：只清理确认无人监听的残留 socket。
func prepareUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("listen: unix %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("listen_invalid: unix 路径 %s 是符号链接，拒绝删除", path)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("listen_invalid: unix 路径 %s 已存在且不是 socket，拒绝删除", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, unixProbeTimeout)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("listen_invalid: unix 路径 %s 已被占用", path)
	}
	// 只有「连接被拒 / 路径消失」可以当作残留；其它错误保守拒绝。
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("listen: unix %s 探测失败（不删除）：%w", path, dialErr)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("listen: unix %s 清理残留 socket：%w", path, err)
	}
	return nil
}

// Endpoint 返回**实际**端点（tcp 的 :0 已解析成内核分配的端口）。
//
// tcp 形态是 `http://host:port`（capability.md §4.0 的 ANOTIFY_EMIT_ENDPOINT）。
// unix 形态沿用配置里的 `unix://<path>`：能力脚本要额外胶水，具体注入形式由
// emit 网关接线时定；监听层只回答「实际落在哪个 socket」。
func (l *Listener) Endpoint() string {
	if l.spec.Network == "unix" {
		return "unix://" + l.unixPath
	}
	return "http://" + l.Addr().String()
}

// Spec 返回解析后的监听形态。
func (l *Listener) Spec() Spec { return l.spec }

// Close 关闭监听；unix socket 只在 inode 仍是我们创建的那个时删除。
func (l *Listener) Close() error {
	err := l.Listener.Close()
	if l.unixPath == "" {
		return err
	}
	if info, statErr := os.Lstat(l.unixPath); statErr == nil && l.unixInfo != nil && os.SameFile(info, l.unixInfo) {
		if rmErr := os.Remove(l.unixPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
			err = rmErr
		}
	}
	return err
}
