// Package app 把 N2A 的配置/缓存闭包、N2B 的运行器、emit 网关、运维帧与 hub 客户端
// 装配成一个可运行的节点进程（N3 的 main 只调用这里的 Serve）。
//
// 目录布局（全部在 dataDir 下，均由节点创建并记录，GC 只碰自己创建的东西）：
//
//	<dataDir>/anode.applied.yaml     机器级 applied（N2A）
//	<dataDir>/capabilities/<digest>/ 不可变产物缓存（N2A）
//	<dataDir>/results/<hex(task)>/<attempt>/  logs/ + 托管产物副本 + meta.json
//	<dataDir>/work/<hex(task)>/<attempt>/     系统创建的任务目录（默认 workDir）
//
// 一个进程一条 WS 连接：配置 5 帧 + 执行 10 帧 + 运维 4 帧 + emit 转发 2 帧共用。
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/applied"
	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/clock"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/emitgw"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/hub"
	"github.com/zzjcool/anotify/cmd/anode/internal/listen"
	"github.com/zzjcool/anotify/cmd/anode/internal/node"
	"github.com/zzjcool/anotify/cmd/anode/internal/ops"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/runner"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Options 是节点装配参数。
type Options struct {
	// Config 是已严格校验的 anode.yaml。
	Config *config.Config
	// DataDir 是节点私有数据目录（applied / capabilities / results / work）。
	DataDir string
	// Version / Commit 是节点自报的版本事实。
	Version string
	Commit  string
	// Clock 是时钟（测试注入假时钟；nil = 真实时间）。
	Clock clock.Clock
	// Dial 是 WS 拨号注入点（测试用假中枢）。
	Dial hub.DialFunc
	// HTTPClient 是产物拉取/上传的注入点（测试用 httptest）。
	HTTPClient *http.Client
	// UpgradeTarget 是要替换的可执行文件（测试注入临时文件；空 = 当前进程）。
	UpgradeTarget string
	// Restart 是升级成功后的重启钩子（N3 main 注入；nil = 不重启）。
	Restart func()
	// Logf 是诊断日志。
	Logf func(format string, args ...any)
}

// App 是装配好的节点。
type App struct {
	opts     Options
	node     *node.Node
	runner   *runner.Runner
	gateway  *emitgw.Gateway
	ops      *ops.Agent
	client   *hub.Client
	listener *listen.Listener
	httpSrv  *http.Server
	results  *resultstore.Store
	workRoot string
	logf     func(format string, args ...any)
}

// Assemble 完成所有装配（不启动网络循环；N3 的 Serve 会调用 Run）。
func Assemble(opts Options) (*App, error) {
	if opts.Config == nil {
		return nil, errors.New("app: 缺少 anode.yaml")
	}
	if opts.DataDir == "" {
		return nil, errors.New("app: 缺少 dataDir")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dirs := layout(opts.DataDir)

	baseline := hostenv.Capture(opts.Config.EnvPassthrough)
	workRoot := dirs.work
	appliedStore := applied.NewStore(opts.DataDir)
	cache := capability.NewCache(dirs.capabilities)
	n, err := node.New(node.Options{
		Config:       opts.Config,
		Store:        appliedStore,
		Cache:        cache,
		Baseline:     baseline,
		Facts:        node.RuntimeFacts(workRoot, ""),
		AgentVersion: opts.Version,
		AgentCommit:  opts.Commit,
	})
	if err != nil {
		return nil, err
	}
	results := resultstore.New(dirs.results, workRoot)

	uploadClient, err := upload.NewClient(opts.Config.Server, opts.Config.Token)
	if err != nil {
		return nil, err
	}
	uploadClient.HTTP = opts.HTTPClient

	listener, err := listen.Open(opts.Config.Listen)
	if err != nil {
		return nil, err
	}

	r, err := runner.New(runner.Options{
		NodeName:        n.Name(),
		LocalVars:       cloneVars(opts.Config.Vars),
		Passthrough:     append([]string(nil), opts.Config.EnvPassthrough...),
		Baseline:        baseline,
		Facts:           node.RuntimeFacts(workRoot, ""),
		Applied:         n.MachineState,
		AllowsNamespace: n.AllowsNamespace,
		LoadArtifact:    n.LoadArtifact,
		Results:         results,
		Upload:          uploadClient,
		EmitEndpoint:    listener.Endpoint(),
		WorkRoot:        workRoot,
		Clock:           opts.Clock,
		Logf:            logf,
	})
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	// A3：hello/config.applied 的槽位事实来自运行器真实账本。
	// （Recover 在 Run 里调用：先恢复重启前的物理占用，再对外申报。）
	n.SetInstanceSource(r.Instances)

	gw := emitgw.New(nil, r)
	gw.Logf = logf
	opsAgent := &ops.Agent{
		Results: results,
		Runner:  r,
		Upgrade: ops.UpgradeConfig{
			ServerURL:  opts.Config.Server,
			NodeToken:  opts.Config.Token,
			TargetPath: opts.UpgradeTarget,
			HTTP:       opts.HTTPClient,
		},
		Restart: opts.Restart,
		Logf:    logf,
	}
	handler := &aggregate{node: n, runner: r, gateway: gw, ops: opsAgent}
	client, err := hub.New(opts.Config.Server, opts.Config.Token, handler)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	client.Dial = opts.Dial
	client.Logf = logf
	client.Handler = handler

	r.SetSender(client)
	gw.Sender = client

	a := &App{
		opts:     opts,
		node:     n,
		runner:   r,
		gateway:  gw,
		ops:      opsAgent,
		client:   client,
		listener: listener,
		results:  results,
		workRoot: workRoot,
		logf:     logf,
	}
	a.httpSrv = &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
	}
	return a, nil
}

// Serve 用配置路径构造并运行节点（N3 main 的唯一入口）。
func Serve(ctx context.Context, opts Options) error {
	a, err := Assemble(opts)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// Node / Runner / Gateway 暴露装配结果（测试与 N3 诊断用）。
func (a *App) Node() *node.Node            { return a.node }
func (a *App) Runner() *runner.Runner      { return a.runner }
func (a *App) Gateway() *emitgw.Gateway    { return a.gateway }
func (a *App) Results() *resultstore.Store { return a.results }
func (a *App) EmitEndpoint() string        { return a.listener.Endpoint() }

// Run 启动 WS 客户端 + emit HTTP 网关 + GC 循环，直到 ctx 取消或认证被拒。
func (a *App) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A3：先从持久 attempt 记录恢复物理槽位（未确认 reaped 的旧进程组保持 busy）。
	if err := a.runner.Recover(); err != nil {
		a.logf("app: 恢复物理槽位失败（按未知处理，不假释放）：%v", err)
	}
	// 启动时先做一次安全 GC（重启恢复：读 meta 判断过期；活动任务来自恢复后的账本）。
	a.CollectGarbage(a.now())

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- a.client.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.httpSrv.Serve(a.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("app: emit 网关: %w", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.gcLoop(runCtx)
	}()

	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-errCh:
	}

	// 收尾：drain（停止授权 + 等进程组回收）→ 关网关/监听 → 等 attempt 收尾。
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.runner.Drain(drainCtx)
	drainCancel()
	gatewayCtx, gatewayCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = a.httpSrv.Shutdown(gatewayCtx)
	gatewayCancel()
	a.gateway.Close()
	_ = a.listener.Close()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	a.runner.Close(closeCtx)
	closeCancel()
	cancel()
	wg.Wait()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// CollectGarbage 执行一次安全 GC（活动任务来自运行器）。
func (a *App) CollectGarbage(now time.Time) []string {
	removed, errs := a.results.Collect(now, a.runner.ActiveTasks())
	for _, err := range errs {
		a.logf("app: GC 跳过/失败: %v", err)
	}
	for _, p := range removed {
		a.logf("app: GC 删除已过期系统路径: %s", p)
	}
	return removed
}

func (a *App) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.CollectGarbage(a.now())
		}
	}
}

func (a *App) now() time.Time { return a.opts.Clock.Now() }

// aggregate 把 hub 的四类入口分发给 node / runner / emit 网关 / ops。
type aggregate struct {
	node    *node.Node
	runner  *runner.Runner
	gateway *emitgw.Gateway
	ops     *ops.Agent
}

func (h *aggregate) Hello() protocol.HelloFrame { return h.node.Hello() }

func (h *aggregate) ApplyWelcome(w protocol.WelcomeFrame) (*protocol.ConfigAppliedFrame, error) {
	return h.node.ApplyWelcome(w)
}

func (h *aggregate) AppliedState() (string, string) { return h.node.AppliedState() }

func (h *aggregate) HandleDeploy(ctx context.Context, f *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame {
	return h.node.HandleDeploy(ctx, f)
}

func (h *aggregate) HandleDispatch(f *protocol.TaskDispatchFrame) { h.runner.HandleDispatch(f) }
func (h *aggregate) HandleLeaseGrant(f *protocol.LeaseGrantFrame) { h.runner.HandleLeaseGrant(f) }
func (h *aggregate) HandleLeaseAck(f *protocol.LeaseAckFrame)     { h.runner.HandleLeaseAck(f) }
func (h *aggregate) HandleResultAck(f *protocol.ResultAckFrame)   { h.runner.HandleResultAck(f) }
func (h *aggregate) HandleCancel(f *protocol.TaskCancelFrame)     { h.runner.HandleCancel(f) }
func (h *aggregate) HandleEmitAck(f *protocol.TaskEmitAckFrame)   { h.gateway.HandleEmitAck(f) }
func (h *aggregate) SessionUp()                                   { h.runner.SessionUp() }

func (h *aggregate) HandleLogsRequest(ctx context.Context, f *protocol.LogsRequestFrame) *protocol.LogsResponseFrame {
	return h.ops.HandleLogsRequest(ctx, f)
}

func (h *aggregate) HandleUpgrade(ctx context.Context, f *protocol.UpgradeFrame, send func(any) error) {
	h.ops.HandleUpgrade(ctx, f, send)
}

type dirs struct {
	capabilities string
	results      string
	work         string
}

func layout(dataDir string) dirs {
	return dirs{
		capabilities: filepath.Join(dataDir, "capabilities"),
		results:      filepath.Join(dataDir, "results"),
		work:         filepath.Join(dataDir, "work"),
	}
}

func cloneVars(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// EnsureDirs 创建节点私有目录（0700），供 N3 main 在装配前调用。
func EnsureDirs(dataDir string) error {
	d := layout(dataDir)
	for _, p := range []string{dataDir, d.capabilities, d.results, d.work} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("app: 创建目录 %s: %w", p, err)
		}
	}
	return nil
}
