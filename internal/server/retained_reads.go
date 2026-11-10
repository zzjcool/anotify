package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 保留域读模型（api-conventions §8.4 / §8.5）。
//
// 这三条端点是 v4 **冻结保留**的现有读模型，不是 v3 扁平资源面：
//
//	GET /v1/stats?namespace=NAME                    工作台统计（事件/任务/设备）
//	GET /v1/orphans?namespace=NAME[&olderThan=&limit=]  派不出去的 pending 任务（#270）
//	GET /v1/namespaces/{ns}/deployments[?nodeId=&capabilityId=]  部署事实（A6）
//
// 为什么必须留着（§8.4「保（不顺手下线）」/ §8.5「路径改名/删端点不是已批准项」）：
//
//   - `/v1/orphans` 是背压（#228）**唯一**的可观测出口：一条永远派不出去的 pending
//     会把 `oldest pending` 只增不减，最终让整个事件入口钉在 503；没有这个视图，
//     运维只能等一条 30 分钟才出现的 ERROR 日志。
//   - `/v1/stats` 与其派生计数是控制台工作台的数据源；把它改名为 ns 子资源不是本轮
//     批准项（`/namespaces/{ns}/stats` 作为 Namespace.status 的镜像仍保留）。
//   - deployments 是 §9「只保留 GET」的事实读模型；写/删已随 v3 部署入口退役。
//
// 三者的错误体统一成 Status（§4），作用域规则与资源域**同一份收口**：
//   - `/stats`、`/orphans` 是 cluster 路径，**必须**显式给 `?namespace=`；
//     缺参数 400（不回退默认 ns），且逐个 ns 过 store.AuthorizeNamespace；
//     `*` 只用于跨 ns **读**汇总（orphans 的诊断需要；stats 的 store 口径拒绝 `*`）。
//   - deployments 走 ns 路径，slug/id 解析与授权沿用 namespace_scope.go。

const (
	// legacyStatsWindowSec 是工作台热力图的取数窗口（近 371 天 ≈ 53 周，与 v3 同口径）。
	legacyStatsWindowSec int64 = 371 * 86400
	// orphansHint 是响应里的处置提示（与 v3 同文案；字段形状本轮不改）。
	orphansHint = "孤儿任务不会被自动清理：确认后用 POST /v1/namespaces/{ns}/tasks/{id}/abandon 显式处置"
)

// retainedReadAPI 是保留域读模型的路由集合（全部会话保护 + no-store）。
type retainedReadAPI struct{ d *serverDeps }

// newRetainedReadAPI 绑定共享依赖。
func newRetainedReadAPI(d *serverDeps) *retainedReadAPI { return &retainedReadAPI{d: d} }

// register 挂上三条保留读端点。
func (a *retainedReadAPI) register(mux *http.ServeMux) {
	if mux == nil || a == nil || a.d == nil {
		return
	}
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, noStore(a.d.session(h)))
	}
	route("GET /v1/stats", a.stats)
	route("GET /v1/orphans", a.orphans)
	route("GET /v1/namespaces/{ns}/deployments", a.deployments)
}

// stats 处理 GET /v1/stats?namespace=NAME。
//
// 形状沿用 v3（`internal/store.Stats`：total/today/delivered/deliveryTotal/deviceCount/daily）：
// §8.4 明确「请求/响应形状本轮不改，只统一错误体」。`*` 在这里是 400：store.MessageStats
// 的口径就没有「跨 ns 汇总」这一项（会把不同命名空间的事件数加成一个不存在的数字）。
func (a *retainedReadAPI) stats(w http.ResponseWriter, r *http.Request) {
	userID, err := requireUser(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	nsID, err := a.d.authorizeQueryNamespace(r, false)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	stats, err := a.d.DB.MessageStats(r.Context(), nsID, userID, taskNowSeconds(a.d)-legacyStatsWindowSec)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// orphans 处理 GET /v1/orphans?namespace=NAME[&olderThan=1h][&limit=200]。
//
// `?namespace=*` 允许（诊断恰恰需要一次看全），但**逐 ns 过授权收口**：先按登录身份
// 逐 ns 调 store.AuthorizeNamespace，再查。缺 namespace → 400（绝不跨 ns 默认全量）。
func (a *retainedReadAPI) orphans(w http.ResponseWriter, r *http.Request) {
	userID, err := requireUser(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	nsID, err := a.d.authorizeQueryNamespace(r, true)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	olderThan, err := parseOlderThan(r.URL.Query().Get("olderThan"))
	if err != nil {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, err.Error()))
		return
	}
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("limit")) != "" && limit <= 0 {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest,
			"limit must be a positive integer"))
		return
	}

	nsIDs := []string{nsID}
	allScopes := nsID == store.AllNamespaces
	if allScopes {
		all, lerr := a.d.DB.ListNamespaces(r.Context())
		if lerr != nil {
			writeAPIError(w, lerr)
			return
		}
		nsIDs = make([]string, 0, len(all))
		for _, ns := range all {
			// 逐 ns 授权：跨 ns 汇总不是绕过收口的后门（今天任何登录用户都能访问任何
			// 存在的 ns；将来按成员表收紧时，这一行就是权限生效的地方）。
			if aerr := a.d.DB.AuthorizeNamespace(r.Context(), userID, ns.ID); aerr != nil {
				writeAPIError(w, aerr)
				return
			}
			nsIDs = append(nsIDs, ns.ID)
		}
	}

	items := make([]store.OrphanPendingTask, 0, 16)
	for _, id := range nsIDs {
		got, lerr := a.d.DB.ListOrphanPendingTasks(r.Context(), id, olderThan, limit)
		if lerr != nil {
			writeAPIError(w, lerr)
			return
		}
		items = append(items, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orphans":   items,
		"count":     len(items),
		"olderThan": olderThan,
		"namespace": nsID,
		"allScopes": allScopes,
		"hint":      orphansHint,
	})
}

// deployments 处理 GET /v1/namespaces/{ns}/deployments：部署事实的只读列表（A6）。
//
// 形状沿用 v3（`{deployments:[…]}`，`store.Deployment` 的 camelCase JSON），过滤参数
// `nodeId` / `capabilityId` 也沿用（§9：只保留 GET，写/删随 v3 部署入口退役）。
func (a *retainedReadAPI) deployments(w http.ResponseWriter, r *http.Request) {
	nsID, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	q := r.URL.Query()
	items, err := a.d.DB.ListDeployments(r.Context(), nsID, store.DeploymentFilter{
		NodeID:       strings.TrimSpace(q.Get("nodeId")),
		CapabilityID: strings.TrimSpace(q.Get("capabilityId")),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if items == nil {
		items = []*store.Deployment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": items})
}

// parseOlderThan 解析 `olderThan`：支持 "1h" / "30m" / "90s" 与纯数字（秒）。
//
// 下限 1 分钟、上限 30 天：下限防误报（刚派发的任务在派发瞬间也可能「暂时没有在线
// 节点」），上限防「用 olderThan=10年 把诊断接口当导出接口」。空值 = 1 小时。
// 逐字沿用 v3（#270）的口径——这是保留读模型，不重设计。
func parseOlderThan(v string) (int, error) {
	const (
		minSec = 60
		maxSec = 30 * 24 * 3600
		defSec = 3600
	)
	v = strings.TrimSpace(v)
	if v == "" {
		return defSec, nil
	}
	sec := 0
	if n, err := strconv.Atoi(v); err == nil {
		sec = n
	} else {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return 0, errors.New("olderThan 需要是时长（如 1h）或秒数")
		}
		sec = int(d.Seconds())
	}
	if sec < minSec {
		return 0, errors.New("olderThan 不能小于 1m（刚派发的任务可能暂时没有在线节点）")
	}
	if sec > maxSec {
		sec = maxSec
	}
	return sec, nil
}
