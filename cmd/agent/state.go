package main

// 本文件是 `<data-dir>/state.json` 的**唯一形状与唯一写者**（issue #320 §2）：
// daemon 每心跳写一次，`anotify-agent status` 只读这个文件。
//
// # 为什么是文件而不是 socket
//
// 现在唯一的本地 socket 是任务期的 emit（ANOTIFY_EMIT_SOCK + 一次性
// ANOTIFY_TASK_TOKEN 鉴权）。给节点开常驻控制面 = 多一个攻击面，而我们要的只是
// 「看一眼它还在不在、连没连上、几个槽位在忙」。一个只写不读的文件就够了：
// CLI 不需要与 daemon 对话，也就不需要鉴权、不需要考虑「老版本 daemon 接了新版本
// CLI 的请求」这类兼容问题。
//
// # 形状的纪律
//
//   - 字段就是 issue §2 列的那些（`sessions` 为 #321 预留，恒为 `[]`，见下）；
//   - `lastError` 为 null 表示「这次启动以来没出过错」，**不做** omitempty：
//     形状稳定比省几个字节重要（jq 读 null 比读「字段不存在」简单）；
//   - 时间是 RFC3339Nano（与 runs/meta.json 同一种格式，读的人不必猜单位）；
//   - `heartbeatSec` 是 issue 字段表之外**多出来的那一个**：stale 判定是
//     「updatedAt 超过 3 个心跳周期」，而心跳周期由 welcome 帧动态给（缺省 10s）。
//     不写进文件，CLI 就只能猜一个固定值——那会在 heartbeat_sec 调小时让状态
//     看起来永远新鲜，调大时又误判成 stale。
//
// # 写失败不得影响节点
//
// 磁盘满、数据目录只读、state.json 被一个同名目录占着：这些都只 Warn（且同一类
// 错误只 Warn 一次，避免每 5 秒刷一行），任务照跑、连接照用。这个文件是**观测**
// 手段，不是节点的运行前提。
//
// # 写是原子的
//
// 走 cache.go 的 writeFileSync（临时文件 → fsync → rename）：CLI 任何时刻读到的
// 都是某一拍的**完整**快照，不会撞见写了一半的 JSON。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// stateFileName 是 daemon 写在数据目录根下的状态快照文件名（契约见文件头）。
	stateFileName = "state.json"

	// stateDirMode / stateFileMode：数据目录本身与状态文件的权限。
	// 0600 是纪律（与 runs/、emit/ 同一档）：里面有 server 地址与错误原文。
	stateDirMode  os.FileMode = 0o755
	stateFileMode os.FileMode = 0o600

	// stateStaleHeartbeats 是 issue §2 的 stale 判定口径：3 个心跳周期没更新就算过期。
	stateStaleHeartbeats = 3

	// stateMinStaleWindow 是 stale 窗口的下限。
	//
	// heartbeat_sec 可以小到 1（契约 §3.4.2 的心跳间隔 = max(1s, heartbeatSec/2)），
	// 3 × 1s = 3s 的窗口会把「一次 GC 抖动 / 系统休眠后的第一拍」误判成 stale。
	// 3s 是「真的没了」与「刚好错过两拍」之间能站住的最小值。
	stateMinStaleWindow = 3 * time.Second

	// stateLastErrorMaxBytes 是 lastError.message 的字节上限（一行）。
	// 错误原文可能很长（dial 失败会带完整 URL 与库的嵌套错误），状态文件不该被它撑爆。
	stateLastErrorMaxBytes = 500
)

// stateSlots 是槽位占用：total 来自节点配置（instances），busy 是正在执行的任务数。
type stateSlots struct {
	Total int `json:"total"`
	Busy  int `json:"busy"`
}

// stateLastError 是「最近一次**节点级**错误」（任务失败不算：那是任务的事实，
// 在 runs/ 里；这里记的是连接、握手、配置这类让节点自己不健康的事）。
type stateLastError struct {
	At      string `json:"at"`      // RFC3339Nano（UTC）
	Kind    string `json:"kind"`    // 与 CLI 错误契约同一份词表（comm_failed / failed …）
	Message string `json:"message"` // 一行（超长按 stateLastErrorMaxBytes 截断）
}

// stateSession 为 #321（hc 托管的 TUI 会话）预留：P2 才填，现在恒为空数组。
//
// 字段先定下来（name/address/status/lastTaskId），免得 P2 改形状时 CLI 与
// daemon 要同时升级——两份二进制在用户机器上永远不同步。
type stateSession struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Status     string `json:"status"`
	LastTaskID string `json:"lastTaskId"`
}

// agentState 是 state.json 的全部内容（issue §2 的字段表 + heartbeatSec）。
//
// 它同时是 daemon 的写形状与 `status --json` 的读形状：一个类型，两处使用，
// 形状不可能分叉。
type agentState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Server    string `json:"server"`
	Connected bool   `json:"connected"`

	Slots stateSlots `json:"slots"`
	// LastError 为 null = 这次启动以来没出过错。
	LastError *stateLastError `json:"lastError"`

	UpdatedAt string `json:"updatedAt"`

	// Sessions 为 #321 预留：现在恒为 `[]`（空列表纪律：不能是 null）。
	Sessions []stateSession `json:"sessions"`

	// HeartbeatSec 是当前心跳周期（秒）：CLI 用它算 stale 窗口（3 个周期）。
	HeartbeatSec int `json:"heartbeatSec"`
}

// statusReport 是 `status --json` 的形状：state.json 原样 + 读的那一刻算出的 stale。
//
// stale 不进文件：它取决于「读的时刻」与「读它的那台机器上 pid 还在不在」，
// 是读者的判断，不是写者的陈述。嵌进 agentState 让字段在同一层平铺。
type statusReport struct {
	agentState
	Stale bool `json:"stale"`
}

// statePath 是状态文件的绝对路径（dataDir 已由 resolveDataDir 归一）。
func statePath(dataDir string) string { return filepath.Join(dataDir, stateFileName) }

// stateTime 是状态文件里时间的唯一格式（UTC、纳秒精度、RFC3339）。
func stateTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// stateWriter 是 state.json 的写者（daemon 侧）。
//
// 并发：心跳 goroutine、任务 goroutine（busy 增减）、读循环（连接断开）都会写，
// 所以全部经 mu 串行化。每次写都整份重写（文件只有几百字节，原子 rename）。
type stateWriter struct {
	path string
	log  *slog.Logger

	mu sync.Mutex
	st agentState
	// lastWriteErr 是上一次写失败的文案：同一类失败只 Warn 一次（心跳每几秒一次，
	// 不 de-dup 会把 journal 刷满，反而看不见别的错误）。
	lastWriteErr string
}

// newStateWriter 建一个指向 dataDir 的写者。
//
// cfg 允许为 nil（单测里的 nodeAgent 可能没有配置）：那样只有 pid/版本这些常量，
// slots.total 为 0。**不**在这里落盘：第一次 start()/touch() 才写。
func newStateWriter(dataDir string, cfg *nodeConfig, log *slog.Logger) *stateWriter {
	if log == nil {
		log = slog.Default()
	}
	w := &stateWriter{
		path: statePath(dataDir),
		log:  log,
		st: agentState{
			PID:          os.Getpid(),
			Version:      buildVersion(),
			Commit:       buildCommit(),
			StartedAt:    stateTime(time.Now()),
			UpdatedAt:    stateTime(time.Now()),
			HeartbeatSec: defaultHeartbeatSec,
			// 空列表纪律：sessions 是 [] 而不是 null（#321 填之前也一样）。
			Sessions: []stateSession{},
		},
	}
	if cfg != nil {
		w.st.Server = cfg.Server
		w.st.Slots.Total = cfg.slots()
	}
	return w
}

// start 落第一份快照：进程起来了，还没连上中枢。
func (w *stateWriter) start() {
	w.update(func(st *agentState) {
		st.PID = os.Getpid()
		st.StartedAt = stateTime(time.Now())
		st.Connected = false
	})
}

// setConnected 记录连接状态（握手成功 → true；连接断开/正常退出 → false）。
//
// heartbeatSec > 0 时一并更新心跳周期（welcome 帧给的值，缺省 10s）。
func (w *stateWriter) setConnected(connected bool, heartbeatSec int) {
	w.update(func(st *agentState) {
		st.Connected = connected
		if heartbeatSec > 0 {
			st.HeartbeatSec = heartbeatSec
		}
	})
}

// touch 刷新 updatedAt（每次心跳调用一次）。
//
// 这就是「daemon 活着」的证据：pid 还在但 updatedAt 太旧，等于进程还在却没在跑
// 心跳循环（卡死 / stop 的进程）——status 会把这种情况判成 stale。
func (w *stateWriter) touch() { w.update(func(*agentState) {}) }

// setBusy 按 delta 增减 busy 槽位（clamp 到 >= 0：一个 bug 不该写出负数）。
func (w *stateWriter) setBusy(delta int) {
	w.update(func(st *agentState) {
		st.Slots.Busy += delta
		if st.Slots.Busy < 0 {
			st.Slots.Busy = 0
		}
	})
}

// fail 记录一次节点级错误（同时把 connected 置 false）。
func (w *stateWriter) fail(kind, message string) {
	w.update(func(st *agentState) {
		st.Connected = false
		st.LastError = &stateLastError{
			At:      stateTime(time.Now()),
			Kind:    kind,
			Message: oneLine(message, stateLastErrorMaxBytes),
		}
	})
}

// stop 标记「这个进程要走了」：连接不再可用，但**不动** lastError
// （正常退出不是错误，别把上次的真实故障擦掉）。
func (w *stateWriter) stop() {
	w.update(func(st *agentState) { st.Connected = false })
}

// update 在锁内改一份快照、刷新 updatedAt、整份落盘。
func (w *stateWriter) update(fn func(*agentState)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(&w.st)
	w.st.UpdatedAt = stateTime(time.Now())
	w.flushLocked()
}

// flushLocked 把当前快照原子地写进 state.json（调用方持锁）。
//
// 字段顺序由结构体定义固定（不排序 map——这里没有 map），同一份状态每次写出
// 逐字节一致，便于 diff 与测试断言。
func (w *stateWriter) flushLocked() {
	if w == nil || w.path == "" {
		return
	}
	data, err := marshalIndent(w.st)
	if err == nil {
		if mkErr := os.MkdirAll(filepath.Dir(w.path), stateDirMode); mkErr != nil {
			err = fmt.Errorf("建数据目录 %s: %w", filepath.Dir(w.path), mkErr)
		} else if werr := writeFileSync(w.path, data, stateFileMode); werr != nil {
			err = fmt.Errorf("写 %s: %w", w.path, werr)
		}
	}
	if err == nil {
		w.lastWriteErr = ""
		return
	}
	// 同一类失败只 Warn 一次（见 lastWriteErr 的注释）。这条永远不影响任务成败。
	if msg := err.Error(); msg != w.lastWriteErr {
		w.lastWriteErr = msg
		w.log.Warn("state snapshot write failed",
			"event", "agent.state.write_failed",
			"path", w.path,
			"error", msg)
	}
}

// loadAgentState 读一份状态快照（status 子命令用；dataDir 已归一）。
//
// 文件不存在时把 fs.ErrNotExist 原样返回：调用方要把它与「文件坏了」分开说
// （前者是「daemon 从没在这台机器上起来过」，后者是「状态文件烂了」）。
func loadAgentState(dataDir string) (*agentState, error) {
	path := statePath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st agentState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if st.Sessions == nil {
		// 手写/旧版本的文件可能没有这个字段：读出来也要是 []（空列表纪律）。
		st.Sessions = []stateSession{}
	}
	return &st, nil
}

// stateStaleWindow 是「3 个心跳周期」的实际时长（下限 stateMinStaleWindow）。
func stateStaleWindow(heartbeatSec int) time.Duration {
	sec := heartbeatSec
	if sec <= 0 {
		sec = defaultHeartbeatSec
	}
	window := time.Duration(sec*stateStaleHeartbeats) * time.Second
	if window < stateMinStaleWindow {
		window = stateMinStaleWindow
	}
	return window
}

// stateIsStale 判定一份快照是否已经不能当真：pid 不在了，或 updatedAt 太旧。
//
// 两条都要看：pid 可能被复用（旧 pid 被别的进程占了），updatedAt 也可能因为
// 一个卡死的进程停在很久以前。任一不成立就判 stale——「这是最后一次已知状态」
// 比「假装它还活着」安全。
func stateIsStale(st *agentState, now time.Time) bool {
	if st == nil {
		return true
	}
	if !processAlive(st.PID) {
		return true
	}
	updated, err := time.Parse(time.RFC3339Nano, st.UpdatedAt)
	if err != nil {
		return true
	}
	return now.Sub(updated) > stateStaleWindow(st.HeartbeatSec)
}

// processAlive 报告 pid 是否还在（signal 0：只探测，不投递）。
//
// EPERM = 进程存在但属于别的用户（也能说明它活着）；ESRCH = 不存在。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// 交给分发（syscall.EPERM）；不依赖 errno 的数字值。
	return errors.Is(err, syscall.EPERM)
}

// stateFileMissing 报告错误是不是「压根没有 state.json」。
func stateFileMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
