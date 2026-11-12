package capability

import (
	"context"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Deployer 把一条**显式** capability.deploy 帧处理成 capability.deployed 回执。
//
// 成功 = 产物已 GET、校验、缓存；失败 = 同一组 refs + status=failed + 人话原因。
// 无论成功失败，回执的 requestId / namespace / name / version / digest 都必须与
// 请求完全一致（中枢按 requestId 配对并用 refs 校验部署目标未被篡改）。
type Deployer struct {
	Fetcher *Fetcher
	Cache   *Cache
	// LookPath 探测 requires.commands（与 hello.capabilities 同源：启动快照的 PATH）。
	// nil = 不探测（回执不带 commands，中枢保持旧判断）。
	LookPath func(name string) (string, error)
}

// Handle 处理一条部署帧。
//
// 只做「这一条 refs」：没有队列、没有重试风暴、没有自动扫描。重连后要重放
// **已明确请求**的未完成部署由中枢决定（CONTRACT rev2 §7），节点不多做。
func (d *Deployer) Handle(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame {
	ack := &protocol.CapabilityDeployedFrame{
		Type:      protocol.FrameCapabilityDeployed,
		RequestID: "",
		Status:    protocol.DeployStatusFailed,
	}
	if frame == nil {
		ack.Error = "空的 capability.deploy 帧"
		return ack
	}
	// 先把 refs 原样抄进回执：失败路径也必须能配对回请求。
	ack.RequestID = frame.RequestID
	ack.DeployRefs = frame.DeployRefs

	fail := func(err error) *protocol.CapabilityDeployedFrame {
		ack.Status = protocol.DeployStatusFailed
		if err != nil {
			ack.Error = oneLine(err)
		}
		return ack
	}
	if frame.Type != protocol.FrameCapabilityDeploy {
		return fail(&Error{Code: CodeBadRef, Message: "帧类型不是 capability.deploy"})
	}
	if d.Fetcher == nil {
		return fail(&Error{Code: CodeBadRef, Message: "未配置产物拉取客户端"})
	}
	if d.Cache == nil {
		return fail(&Error{Code: CodeBadRef, Message: "未配置产物缓存"})
	}
	a, digest, err := d.Fetcher.Fetch(ctx, frame.DeployRefs)
	if err != nil {
		return fail(err)
	}
	if _, err := d.Cache.Store(a, digest); err != nil {
		return fail(err)
	}
	ack.Status = protocol.DeployStatusApplied
	ack.Error = ""
	ack.Commands = d.probeCommands(a)
	return ack
}

// probeCommands 探测能力的 requires.commands（与 capabilitiesLocked 同一口径：
// 只用启动快照的 PATH，不读 os.Getenv）。
func (d *Deployer) probeCommands(a *artifact.Artifact) []protocol.HelloCommand {
	if d.LookPath == nil || a == nil {
		return nil
	}
	required := a.Spec.Requires.Commands
	if len(required) == 0 {
		return nil
	}
	out := make([]protocol.HelloCommand, 0, len(required))
	for _, name := range required {
		cmd := protocol.HelloCommand{Name: name}
		if path, err := d.LookPath(name); err == nil {
			cmd.Found = true
			cmd.Path = path
		}
		out = append(out, cmd)
	}
	return out
}

// oneLine 把人话原因压成一行（回执 error 不参与判断，但不能带换行污染日志）。
func oneLine(err error) string {
	s := err.Error()
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
