// Package trigger 实现「外部世界的变化 → 事件行 → 首次派发」的合流点。
//
// 三类触发器（webhook / cron / watch）产出的东西完全一致：
//
//	source + type + payload + traceId  →  events 行  →  Dispatcher.Dispatch
//
// 差异只在**什么时候**产生事件、以及 trace_id 长什么样：
//
//	webhook  收到 HTTP 请求时         trace_<时间戳>_<随机>
//	cron     到点触发时               trace_cron_<name>_<fire_time>
//	watch    检测到变化时             trace_<时间戳>_<随机>
//
// 契约要点：
//   - trace_id **由触发器生成**，中枢不代生成（§10 Q2）
//   - 幂等键由触发器给（`idemKey`），中枢只做唯一约束（§10.1）
//   - 带 idemKey 冲突时返回**已有**的 event_id + trace_id，不是 409
//   - 起点是 origin 能力（无节点声明）：它没有任务行，只有出口
package trigger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// Store 是触发器需要的最小 store 表面（便于测试注入）。
//
// *store.DB 满足它。
//
// 参数名都是 namespaceID：命名空间是唯一隔离维度，也是唯一作用域参数（契约 §2.1）。
type Store interface {
	InsertEvent(ctx context.Context, e *store.Event) (bool, error)
	ListCapabilities(ctx context.Context, namespaceID string) ([]*store.CapabilityVersion, error)
	EnsureGraphVersion(ctx context.Context, namespaceID string) (*store.GraphVersion, error)
}

// Dispatcher 是派发器（route.Dispatcher 满足这个接口）。
type Dispatcher interface {
	Dispatch(ctx context.Context, in route.DispatchInput) (*route.DispatchOutput, error)
}

// Service 是触发器服务：入事件 + 触发首轮派发。
type Service struct {
	Store      Store
	Dispatcher Dispatcher

	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64
	// NewTraceID 生成 trace_id（测试可注入以断言「由触发器生成」）。
	NewTraceID func() string
}

// New 构造触发器服务。
func New(st Store, d Dispatcher) *Service {
	return &Service{
		Store:      st,
		Dispatcher: d,
		Now:        store.Now,
		NewTraceID: NewTraceID,
	}
}

// IngestRequest 是一次事件摄入请求（对应 POST /v1/events 的 body）。
type IngestRequest struct {
	// NamespaceID 是事件落进哪个命名空间（**必填**）。
	//
	// 事件属于命名空间（I10）：入口边、trace 列表、统计全按它过滤。
	// 缺了它就没有「这条事件属于哪套流程」的答案，所以直接报错——
	// 绝不回退默认命名空间（契约 §6.2）。
	NamespaceID string `json:"namespaceId"`
	// Source 是事件来源，如 "github" / "cron" / "watch"。
	Source string `json:"source"`
	// Type 是事件类型，如 "pull_request.opened"。**它就是 origin 能力的出口名**。
	Type string `json:"type"`
	// IdemKey 可选幂等键（GitHub 用 X-GitHub-Delivery，cron 用 cron:<name>:<fire_time>）。
	// 为 nil 表示这个来源没有稳定投递 ID —— 允许重复（契约 §10.1）。
	IdemKey *string `json:"idemKey,omitempty"`
	// TraceID 可选：给了就用它（同一 trace 的事件串起来），不给则由触发器生成。
	TraceID string `json:"traceId,omitempty"`
	// Payload 是事件原始负载。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// IngestResult 是事件摄入的结果。
type IngestResult struct {
	Event *store.Event `json:"event"`
	// Inserted=false 表示命中幂等，返回的是**已有**的事件（event_id + trace_id 都来自旧行）。
	Inserted bool `json:"inserted"`
	// Dispatch 是首轮派发的结果（幂等命中时为空——事件已经处理过了）。
	Dispatch *route.DispatchOutput `json:"dispatch,omitempty"`
	// OriginCapability 是匹配到的起点能力（source@version），nil 表示没找到定义。
	OriginCapability *store.CapabilityVersion `json:"-"`
}

// ErrUnknownOrigin 表示 source 没有对应的 origin 能力定义。
//
// 这是**调用方的错误**，不是"事件先存后补"的场景：
// 没有起源能力的事件永远派不出去，存下来只是占库位。
// 所以 HTTP 层应该返回 422 并告诉用户有哪些可用的 source，
// 而不是 200 + 空 origin（那会让人以为成功了，实际什么都没发生）。
var ErrUnknownOrigin = errors.New("unknown origin capability for source")

// UnknownOriginError 携带可用 source 列表，供 HTTP 层写进错误信息。
//
// 用户看到 "未知来源 github-ee；可用：github、cron、http、manual"
// 远比看到 "unknown origin capability for source" 有用——
// 前者直接告诉下一步该填什么。
type UnknownOriginError struct {
	Source    string
	Available []string
}

func (e *UnknownOriginError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("unknown origin %q; no capabilities defined yet", e.Source)
	}
	return fmt.Sprintf("unknown origin %q; available: %s",
		e.Source, strings.Join(e.Available, ", "))
}

// Unwrap 让 errors.Is(err, ErrUnknownOrigin) 仍然成立。
func (e *UnknownOriginError) Unwrap() error { return ErrUnknownOrigin }

// Ingest 把一次外部变化变成事件行，并触发第一次派发（契约 §2.5 / §10.1）。
//
// 顺序（先落库，再派发）：
//
//  1. 确定 trace_id：给了就用，没给由触发器生成
//  2. 幂等写 events（带 idemKey 时冲突返回已有行，不是 409）
//  3. 幂等命中 → 直接返回（事件已处理过，不重复派发）
//  4. 查 source 对应的 origin 能力定义（source@latest）
//  5. 用 (origin_capability_id, type) 作为出口名调 Dispatch
//
// 第 4 步找不到定义时返回 ErrUnknownOrigin：事件已入库，只是没法派发。
func (s *Service) Ingest(ctx context.Context, req *IngestRequest) (*IngestResult, error) {
	if req == nil {
		return nil, fmt.Errorf("ingest: nil request")
	}
	if req.NamespaceID == "" {
		// 不回退默认命名空间（契约 §6.2）：回退等于把人写错的地方藏起来。
		return nil, fmt.Errorf("ingest: empty namespace_id: %w", store.ErrNamespaceRequired)
	}
	if req.Source == "" {
		return nil, fmt.Errorf("ingest: source is required")
	}
	if req.Type == "" {
		return nil, fmt.Errorf("ingest: type is required")
	}

	// ① trace_id：由触发器生成（契约 §10 Q2）。调用方给了就沿用
	//（同一 trace 的多个事件串起来时，发起方才有全局视角）。
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		traceID = s.newTraceID()
	}

	payload := req.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}

	ev := &store.Event{
		NamespaceID: req.NamespaceID,
		TraceID:     traceID,
		Source:      req.Source,
		Type:        req.Type,
		Payload:     payload,
		IdemKey:     normalizeIdemKey(req.IdemKey),
		CreatedAt:   s.now(),
	}

	// ② 幂等写
	inserted, err := s.Store.InsertEvent(ctx, ev)
	if err != nil {
		return nil, fmt.Errorf("ingest: insert event: %w", err)
	}

	result := &IngestResult{Event: ev, Inserted: inserted}

	// ③ 幂等命中：事件已经处理过了，不重复派发。
	// 仍然返回 200 + 已有的 event_id/trace_id —— 对发送方而言都是成功。
	if !inserted {
		return result, nil
	}

	// ④ 首轮派发：入口边的左端就是**来源名**，出口名就是事件 type。
	//
	// 这里原来要先 findOrigin 找一个叫 github@1 的"起点能力"当锚点。
	// 那种能力不执行代码、不产生任务、也没地方存配置（cron 表达式之类），
	// 却必须存在、而且会出现在能力列表上 —— 源（sources）已经是它的
	// 完整替代，所以入口边直接以来源名为左端（契约 §2.4）。
	out, err := s.Dispatcher.Dispatch(ctx, route.DispatchInput{
		NamespaceID:  req.NamespaceID,
		TraceID:      traceID,
		FromCap:      req.Source,
		FromIsSource: true,
		Out:          req.Type,
		Params:       extractParams(payload),
		// 入口没有父任务（parent_task_id 留空）
		// path 为空：来源不是能力，不参与「A→B→A」环检测
		Path: nil,
	})
	if err != nil {
		// 事件已入库，派发失败不该让发送方重投（重投也还是同一条事件）。
		// 返回事件 + 包装后的错误，由上层决定状态码与重试。
		return result, fmt.Errorf("ingest: dispatch: %w", err)
	}
	result.Dispatch = out
	return result, nil
}

// findOrigin 找 source 对应的 origin 能力定义（取该 name 的最高版本）。
//
// 约定：事件来源 github 对应能力 github@<latest>。
// 取最高版本的理由：capability_versions 是 append-only（不变量 I8），
// 新版本 supersede 旧的；触发器不该钉在某个老版本上。
//
// 注意不能用 GetCapability(version=0)：那是精确匹配 version=0，永远查不到东西。
func (s *Service) findOrigin(ctx context.Context, namespaceID, source string) (*store.CapabilityVersion, error) {
	all, err := s.Store.ListCapabilities(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("ingest: list capabilities: %w", err)
	}
	var best *store.CapabilityVersion
	for _, c := range all {
		if c.Name != source {
			continue
		}
		if best == nil || c.Version > best.Version {
			best = c
		}
	}
	if best == nil {
		// 把**能当起点**的能力名列给调用方——这是"下一步该做什么"的信息。
		//
		// 不能把中枢执行的终端能力（notify.phone）列进去：它没有节点、
		// outputs 为空，按 DeriveKind 也会被算成 origin，但拿它当事件来源
		// 毫无意义（source=notify.phone 的事件该派给谁？）。
		// 列错了比不列更糟——用户会照着提示填一个错的值。
		available := make([]string, 0, len(all))
		seen := map[string]bool{}
		for _, c := range all {
			if seen[c.Name] {
				continue
			}
			if bc, ok := store.IsBuiltinCapability(c.Name, c.Version); ok && !bc.Origin {
				continue // 中枢执行的内置能力（notify.phone）：不能当起点
			}
			seen[c.Name] = true
			available = append(available, c.Name)
		}
		sort.Strings(available)
		return nil, &UnknownOriginError{Source: source, Available: available}
	}
	return best, nil
}

// normalizeIdemKey 把空白串视为「没有幂等键」。
//
// 空字符串作为 idem_key 是危险的：它会让所有「带了空键」的事件被判为同一条。
func normalizeIdemKey(k *string) *string {
	if k == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*k)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// extractParams 从 payload 里取出可传给下游的参数。
//
// 不做任何转换/求值（不变量 I6）：payload 是对象就整个作为 params，
// 其他类型（数组/字符串）包一层 {"payload": ...}。
func extractParams(payload json.RawMessage) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err == nil && obj != nil {
		return obj
	}
	var anyVal any
	if err := json.Unmarshal(payload, &anyVal); err != nil {
		return map[string]any{}
	}
	return map[string]any{"payload": anyVal}
}

func (s *Service) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return store.Now()
}

func (s *Service) newTraceID() string {
	if s.NewTraceID != nil {
		return s.NewTraceID()
	}
	return NewTraceID()
}

// NewTraceID 生成一个 trace_id：trace_<unixnano>_<8 字节随机 hex>。
//
// trace_id 由**触发器**生成（契约 §10 Q2），不是中枢。
// 用纳秒时间戳 + 随机后缀：时间部分便于人肉排序排查，
// 随机后缀保证同一纳秒内的并发请求不会撞（同 #55 的 cap_/grf_ ID 教训）。
func NewTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败意味着系统熵源不可用；trace_id 是日志关联的关键字段，
		// 静默降级成纯时间戳会在并发下产生重复 trace——宁可 panic。
		panic("trigger: crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("trace_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// CronTraceID 给 cron 触发器生成确定性 trace_id：cron:<name>:<fire_time>。
//
// 用确定性 ID（而不是随机）的理由：同一个 cron 同一时刻重复触发
// 应该被幂等键挡住，而不是产生两条 trace。配合 IdemKey 使用：
//
//	idemKey  = cron:<name>:<fire_time>
//	traceId  = 同上（或省略，由触发器生成随机 ID）
//
// 这里提供 trace 版本，便于把「同一个 cron 的多次触发」聚到一条 trace 上排查。
func CronTraceID(name string, fireTime int64) string {
	return fmt.Sprintf("cron:%s:%d", name, fireTime)
}

// CronIdemKey 是 cron 的幂等键（契约 §10.1 的示例）。
func CronIdemKey(name string, fireTime int64) string {
	return CronTraceID(name, fireTime)
}

// ParsedFireTime 把一个时间解析成 cron 的 fire_time（秒）。
func ParsedFireTime(t time.Time) int64 { return t.Unix() }
