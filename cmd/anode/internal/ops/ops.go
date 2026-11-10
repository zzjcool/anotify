// Package ops 是节点保留运维帧的真实实现：本机日志档案读取（logs_request）与
// 自我二进制升级（upgrade）。
//
// 两条纪律：
//   - logs_response 必须自己按帧上限截断（截的是**回包**，本机档案完整）；
//     配对只靠 requestId，taskId/attempt 必须回实际情况，无档案不伪造空成功。
//   - upgrade 只从中枢根下载（同源，拒绝跳到别处）、sha256 逐字节校验、原子替换
//     目标二进制后回**实际生效**版本。Linux 上真的是替换，不拿 not_supported 遮。
package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/protocol"
)

// maxUpgradeBytes 是下载二进制的安全上限（15 MB 量级 + 余量）。
const maxUpgradeBytes int64 = 200 << 20

// UpgradeTimeout 是一次升级下载 + 替换的总等待上限。
const UpgradeTimeout = 30 * time.Second

// Drainer 是升级/退出前的 drain 入口（runner 实现）。
type Drainer interface {
	Drain(ctx context.Context)
}

// UpgradeConfig 是自我升级的注入点（测试用临时可执行文件 + httptest，不碰宿主）。
type UpgradeConfig struct {
	// ServerURL 是中枢地址（ws:// / wss://；下载走 http:// / https://）。
	ServerURL string
	// NodeToken 是节点身份令牌（Authorization: Bearer）。
	NodeToken string
	// TargetPath 是要替换的可执行文件；空 = 当前进程的 os.Executable()。
	TargetPath string
	// HTTP 是可注入的下载客户端（测试用）。
	HTTP *http.Client
	// DrainTimeout 是升级前停止授权/等进程组回收的上限（默认 30s）。
	DrainTimeout time.Duration
}

// Agent 处理运维帧。
type Agent struct {
	upgradeMu sync.Mutex
	Results   *resultstore.Store
	Runner    Drainer
	Upgrade   UpgradeConfig
	// Restart 是替换成功并 ack 之后的重启钩子（N3 main 注入；测试里是 fake）。
	Restart func()
	Logf    func(format string, args ...any)
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// HandleLogsRequest 读取 (taskId, attempt) 的本机档案并把一帧编码回中枢。
//
// 语义（L1-R3/R4）：
//   - 请求先过 closed-set 校验，非法参数回 illegal_log_request（不归一化）。
//   - attempt=0（帧里省略）表示该逻辑 task 最近一次有档案的 attempt；成功响应必须
//     回实际 attempt，错误 / 无档案不能伪造 latest。
//   - 成功响应带真实结构化记录 Lines（零条 = []）与 raw 尾部窗口；任何记录级/
//     raw 级/帧级截断都只通过 truncated 表达。
//   - 档案存在但索引缺失/损坏/版本不支持/写入失败 → logs_unreadable，绝不空成功。
func (a *Agent) HandleLogsRequest(ctx context.Context, f *protocol.LogsRequestFrame) *protocol.LogsResponseFrame {
	if f == nil || f.Type != protocol.FrameLogsRequest {
		return nil
	}
	resp := &protocol.LogsResponseFrame{
		Type:      protocol.FrameLogsResponse,
		TaskID:    f.TaskID,
		RequestID: f.RequestID,
	}
	if err := protocol.ValidateLogsRequest(f); err != nil {
		resp.Error = protocol.LogsErrIllegalRequest
		a.logf("ops: 非法日志请求 task=%q attempt=%d tailLines=%d sinceSeconds=%d: %v",
			f.TaskID, f.Attempt, f.TailLines, f.SinceSeconds, err)
		return resp
	}
	if a.Results == nil {
		resp.Error = protocol.LogsErrNoLocalArchive
		return resp
	}
	arch, found, err := a.Results.ReadLogs(f.TaskID, f.Attempt, resultstore.LogQuery{
		SinceSeconds: f.SinceSeconds,
		Now:          time.Now().UTC(),
		TailLines:    f.TailLines,
	})
	if err != nil {
		resp.Error = protocol.LogsErrUnreadable
		a.logf("ops: 日志档案不可读 task=%s attempt=%d: %v", f.TaskID, f.Attempt, err)
		return resp
	}
	if !found {
		resp.Error = protocol.LogsErrNoLocalArchive
		return resp
	}
	resp.Attempt = arch.Attempt
	resp.SetLogLines(arch.Lines)
	fitLogsResponse(resp, arch)
	return resp
}

// fitLogsResponse 把「结构化记录 + raw 尾部窗口」装进一帧 ≤ LogsResponseMaxBytes。
//
// 顺序固定为 R4：since 过滤与 tailLines 已在 resultstore 完成；这里先最大化能完整
// 传输的**最新**记录（此时 raw 置空，避免 raw 挤掉记录），再把剩余预算分给 raw
// 尾部窗口。任何一边被丢都只通过 truncated 表达（本机档案本身完整，不动）。
func fitLogsResponse(resp *protocol.LogsResponseFrame, arch resultstore.LogArchive) {
	lines := resp.LogLines()
	keep := maxFittingLines(resp, lines)
	lineDropped := keep < len(lines)

	stdout, stderr, rawDropped := fitRawWithinLimit(resp, arch.Stdout, arch.Stderr)
	resp.Stdout, resp.Stderr = stdout, stderr
	resp.Bytes = int64(len(stdout) + len(stderr))
	resp.Truncated = arch.StdoutMore || arch.StderrMore || arch.Omitted || arch.Partial ||
		lineDropped || rawDropped
}

// maxFittingLines 在 raw 置空的前提下最大化可完整装下的最新记录条数。
//
// 编码体积对「保留条数」单调：丢掉更早的记录只会变小，所以可以二分。
func maxFittingLines(resp *protocol.LogsResponseFrame, lines []protocol.LogLine) int {
	if tryLogsFrame(resp, lines, len(lines), "", "") {
		return len(lines)
	}
	lo, hi, best := 0, len(lines), 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if tryLogsFrame(resp, lines, mid, "", "") {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	_ = tryLogsFrame(resp, lines, best, "", "")
	return best
}

// fitRawWithinLimit 在已选定的记录集合下最大化 raw 尾部窗口。
//
// 两条流用同一个字节预算（与既有 raw 截断行为一致），取各自尾部并保持 UTF-8
// 起点对齐；被丢掉的字节只由 truncated 说话，档案本身不动。
func fitRawWithinLimit(resp *protocol.LogsResponseFrame, stdout, stderr []byte) (string, string, bool) {
	lines := resp.LogLines()
	if tryLogsFrame(resp, lines, len(lines), string(stdout), string(stderr)) {
		return resp.Stdout, resp.Stderr, false
	}
	maxLen := len(stdout)
	if len(stderr) > maxLen {
		maxLen = len(stderr)
	}
	lo, hi := 0, maxLen
	best := -1
	var bestOut, bestErr string
	for lo <= hi {
		mid := (lo + hi) / 2
		out := tailString(stdout, mid)
		errOut := tailString(stderr, mid)
		if tryLogsFrame(resp, lines, len(lines), out, errOut) {
			best = mid
			bestOut, bestErr = out, errOut
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		// 连空 raw 都装不下（元数据自身超限）：不回任何内容，只让 truncated 说话。
		_ = tryLogsFrame(resp, lines, len(lines), "", "")
		return resp.Stdout, resp.Stderr, len(stdout)+len(stderr) > 0
	}
	_ = tryLogsFrame(resp, lines, len(lines), bestOut, bestErr)
	dropped := len(bestOut) < len(stdout) || len(bestErr) < len(stderr)
	return bestOut, bestErr, dropped
}

// tryLogsFrame 以候选记录尾部 / raw 尾部编码一次，报告是否仍 ≤ 帧上限。
//
// bytes 也参与编码（先写再量），避免「截断后反而变长」。
func tryLogsFrame(resp *protocol.LogsResponseFrame, lines []protocol.LogLine, keep int, stdout, stderr string) bool {
	if keep < 0 {
		keep = 0
	}
	if keep > len(lines) {
		keep = len(lines)
	}
	tail := lines[len(lines)-keep:]
	if tail == nil {
		tail = []protocol.LogLine{}
	}
	resp.SetLogLines(tail)
	resp.Stdout, resp.Stderr = stdout, stderr
	resp.Bytes = int64(len(stdout) + len(stderr))
	raw, err := protocol.Encode(resp)
	if err != nil {
		return false
	}
	return len(raw) <= protocol.LogsResponseMaxBytes
}

// tailString 返回 data 的尾部最多 max 字节，并对齐到合法 UTF-8 起点。
func tailString(data []byte, max int) string {
	if max <= 0 {
		return ""
	}
	if len(data) <= max {
		return string(data)
	}
	start := len(data) - max
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	for start < len(data) && !utf8.Valid(data[start:]) {
		start++
	}
	if start >= len(data) {
		return ""
	}
	return string(data[start:])
}

// SendFunc 是 hub 提供的并发安全上行发送入口（升级 ack 用）。
type SendFunc func(frame any) error

// HandleUpgrade 处理 upgrade：drain → 下载 → 校验 → 原子替换 → ack 实际版本 → 重启。
//
// 失败**绝不重启、绝不换掉旧二进制**；错误码用协议封闭集合（need_root / bad_sha /
// download_failed / not_supported）。
func (a *Agent) HandleUpgrade(ctx context.Context, f *protocol.UpgradeFrame, send SendFunc) {
	if f == nil || f.Type != protocol.FrameUpgrade || send == nil {
		return
	}
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	ack := a.apply(ctx, f)
	if ack.Error == "" {
		ack.OK = true
	}
	if err := send(ack); err != nil {
		a.logf("ops: 发送 upgrade_ack 失败：%v", err)
		return
	}
	if ack.OK && a.Restart != nil {
		a.logf("ops: 已替换二进制到 %s，触发重启", a.target())
		a.Restart()
	}
}

func (a *Agent) apply(ctx context.Context, f *protocol.UpgradeFrame) *protocol.UpgradeAckFrame {
	ack := &protocol.UpgradeAckFrame{Type: protocol.FrameUpgradeAck}
	fail := func(code, message string) *protocol.UpgradeAckFrame {
		ack.OK = false
		ack.Error = code
		a.logf("ops: upgrade 失败 %s: %s", code, message)
		return ack
	}
	sha := strings.ToLower(strings.TrimSpace(f.SHA256))
	if !protocol.ValidArtifactSHA256(sha) {
		return fail(protocol.UpgradeErrBadSHA, fmt.Sprintf("sha256 形状不合法（%q）", f.SHA256))
	}
	target, err := a.targetPath()
	if err != nil {
		return fail(protocol.UpgradeErrNotSupported, err.Error())
	}
	endpoint, err := a.downloadURL(f.URLPath)
	if err != nil {
		return fail(protocol.UpgradeErrDownloadFailed, err.Error())
	}

	// 1) drain：停止授权 / 等进程组回收（有界；超时只记录，不阻止替换）。
	if a.Runner != nil {
		drainCtx, cancel := context.WithTimeout(ctx, a.drainTimeout())
		a.Runner.Drain(drainCtx)
		drainErr := drainCtx.Err()
		cancel()
		if drainErr != nil {
			return fail(protocol.UpgradeErrDownloadFailed, "drain 超时，未确认进程组回收，不替换")
		}
		if active, ok := a.Runner.(interface{ ActiveTasks() map[string]bool }); ok && len(active.ActiveTasks()) != 0 {
			return fail(protocol.UpgradeErrDownloadFailed, "尚有未回收物理槽位，不替换")
		}
	}

	// 2) 下载到目标同目录的临时文件（原子 rename 的前提）。
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".anode-upgrade-*")
	if err != nil {
		if isPermission(err) {
			return fail(protocol.UpgradeErrNeedRoot, fmt.Sprintf("目标目录不可写 %s: %v", dir, err))
		}
		return fail(protocol.UpgradeErrDownloadFailed, err.Error())
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	got, err := a.download(ctx, endpoint, tmp)
	if err != nil {
		if isPermission(err) {
			return fail(protocol.UpgradeErrNeedRoot, err.Error())
		}
		return fail(protocol.UpgradeErrDownloadFailed, err.Error())
	}
	if got != sha {
		return fail(protocol.UpgradeErrBadSHA,
			fmt.Sprintf("sha256 不符：下载 %s，声明 %s（已丢弃，%s 未改动）", got, sha, target))
	}
	if err := tmp.Sync(); err != nil {
		return fail(protocol.UpgradeErrDownloadFailed, fmt.Sprintf("fsync 下载文件: %v", err))
	}
	if err := tmp.Close(); err != nil {
		return fail(protocol.UpgradeErrDownloadFailed, fmt.Sprintf("关闭下载文件: %v", err))
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fail(protocol.UpgradeErrDownloadFailed, fmt.Sprintf("chmod 0755: %v", err))
	}
	// 在 rename 前运行下载文件的实际 --version 自检；无效文件不能破坏旧二进制。
	version, commit, err := executableVersion(ctx, tmpName)
	if err != nil {
		return fail(protocol.UpgradeErrDownloadFailed, fmt.Sprintf("二进制自检: %v", err))
	}
	if err := verifyFile(tmpName, sha); err != nil {
		return fail(protocol.UpgradeErrBadSHA, fmt.Sprintf("自检后下载文件变化: %v", err))
	}
	// 3) 原子替换。失败不留下半新半旧：临时文件随后删除。
	if err := os.Rename(tmpName, target); err != nil {
		if isPermission(err) {
			return fail(protocol.UpgradeErrNeedRoot, fmt.Sprintf("替换 %s 需要权限: %v", target, err))
		}
		return fail(protocol.UpgradeErrDownloadFailed, fmt.Sprintf("替换 %s: %v", target, err))
	}
	keep = true
	// 4) 自检：替换后的文件确实是刚下载的那份（大小 + 内容摘要）。
	if err := verifyFile(target, sha); err != nil {
		return fail(protocol.UpgradeErrBadSHA, fmt.Sprintf("替换后自检失败: %v", err))
	}
	ack.Version = version
	ack.Commit = commit
	a.logf("ops: upgrade 成功 target=%s version=%s", target, version)
	return ack
}

// download 把 endpoint 的字节流写进 tmp，返回实际 sha256（64 位 hex）。
func (a *Agent) download(ctx context.Context, endpoint string, dst *os.File) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, UpgradeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if a.Upgrade.NodeToken != "" {
		req.Header.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+a.Upgrade.NodeToken)
	}
	client := http.Client{Timeout: UpgradeTimeout}
	if a.Upgrade.HTTP != nil {
		client = *a.Upgrade.HTTP
	}
	origin, _ := url.Parse(endpoint)
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != origin.Scheme || next.URL.Host != origin.Host || len(via) >= 10 {
			return errors.New("拒绝跨源/过多升级重定向")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s 返回 %d（期望 200）", endpoint, resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxUpgradeBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxUpgradeBytes {
		return "", fmt.Errorf("下载内容超过 %d 字节上限", maxUpgradeBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyFile 重算目标文件摘要（替换后的自检）。
func verifyFile(target, want string) error {
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("内容 sha256=%s，期望 %s", got, want)
	}
	return nil
}

// downloadURL 只允许从中枢根下载（相对路径拼根；绝对 URL 必须同源）。
func (a *Agent) downloadURL(urlPath string) (string, error) {
	base, err := capability.HTTPBaseURL(a.Upgrade.ServerURL)
	if err != nil {
		return "", err
	}
	root, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(urlPath)
	if p == "" {
		p = protocol.NodeBinaryPath
	}
	ref, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("urlPath %q: %w", p, err)
	}
	if ref.IsAbs() {
		if ref.Scheme != root.Scheme || ref.Host != root.Host {
			return "", fmt.Errorf("urlPath %q 与中枢不同源，拒绝下载可执行文件", p)
		}
		return ref.String(), nil
	}
	resolved := root.ResolveReference(ref)
	if resolved.Scheme != root.Scheme || resolved.Host != root.Host || resolved.User != nil {
		return "", errors.New("拒绝跨源升级 URL")
	}
	return resolved.String(), nil
}

// targetPath 解析要替换的可执行文件（可注入；默认当前进程）。
func (a *Agent) targetPath() (string, error) {
	if a.Upgrade.TargetPath != "" {
		return filepath.Abs(a.Upgrade.TargetPath)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法确定当前可执行文件: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("解析 %s: %w", exe, err)
	}
	return resolved, nil
}

func (a *Agent) target() string {
	target, err := a.targetPath()
	if err != nil {
		return "?"
	}
	return target
}

func (a *Agent) drainTimeout() time.Duration {
	if a.Upgrade.DrainTimeout > 0 {
		return a.Upgrade.DrainTimeout
	}
	return 30 * time.Second
}

func isPermission(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.EACCES)
}

// boundedOutput prevents a downloaded self-test from filling memory with stdout/stderr.
type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("self-test output exceeds 4096 bytes")
	}
	return b.Buffer.Write(p)
}
func executableVersion(ctx context.Context, path string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Dir(path)}
	cmd.WaitDelay = time.Second
	var stdout, stderr boundedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("执行 --version: %w", err)
	}
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		return "", "", fmt.Errorf("版本 JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", "", errors.New("版本输出必须一个 JSON 对象")
	}
	if strings.TrimSpace(info.Version) == "" || strings.TrimSpace(info.Commit) == "" {
		return "", "", errors.New("版本/commit 必填")
	}
	return info.Version, info.Commit, nil
}
