package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是**部署应用层**：处理中枢下发的 deploy 帧（新增 / 撤销一份产物）。
//
// 三条纪律（契约 §4 / §5）：
//
//  1. **部署失败是业务结果，不是协议错误**：一律走 apply 帧的 reason 短码，
//     绝不回 error 帧。error 帧是「你的帧坏了 / 你不该发这个」，中枢收到会当成
//     节点故障；而「这台机器没装 claude」是**正常**的部署结论，必须逐台显示在
//     控制台上，而不是把连接掀掉。
//  2. **落盘要原子**（cache.put）：写一半的产物等于节点声称能跑、实际跑不了，
//     正是本 epic 要消灭的「两个真相源」。
//  3. **成功之后重发 hello**：中枢靠 hello.applied 全量重建 node_capabilities
//     （裁决 D4）。节点缓存变了却不说，中枢只能等下次重连才发现——控制台上
//     「已部署」会看起来永远停在 pending。
//
// 为什么部署在这一层同步做（不扔 goroutine）：产物最大 1 MiB，落盘是毫秒级；
// 同步做的好处是「同一连接上 deploy 与 assign 的先后顺序」天然确定——中枢推完
// deploy 立刻派单时，缓存一定已经可读。

// handleDeploy 处理一帧 deploy，返回**传输层**错误（发不出去）。
//
// 部署本身失败（产物非法 / 缺依赖 / 写盘失败）不是 error：它已经写进 apply 帧的
// reason 里了。返回非 nil 只表示「我没能告诉中枢结果」，此时读循环会记 warn，
// 连接坏了由读循环自己收尸。
func (a *nodeAgent) handleDeploy(ctx context.Context, f *deployFrame) error {
	if f == nil {
		return nil
	}
	// 连接正在收尾：不再改本地缓存（这一帧没 ack 掉，中枢重连后会对账重推）。
	if err := ctx.Err(); err != nil {
		return err
	}

	switch f.Action {
	case ws.DeployActionApply:
		return a.applyArtifact(f)
	case ws.DeployActionRemove:
		return a.removeArtifact(f)
	default:
		// 未知 action：明确回 unsupported_action，绝不猜。
		// 猜错的代价不对称——猜成 apply 会覆盖本地变体，猜成 remove 会删掉一份产物。
		a.log.Warn("deploy with unsupported action",
			"event", "agent.deploy.unsupported_action",
			"deploy_id", f.DeployID, "action", f.Action)
		return a.replyApply(&applyFrame{
			DeployID: f.DeployID,
			State:    ws.ApplyStateFailed,
			Reason:   ws.ApplyReasonUnsupportedAction + ": " + f.Action,
		})
	}
}

// applyArtifact 落一份产物：校验 → 探依赖 → 原子写 → 回 applied → 重发 hello。
func (a *nodeAgent) applyArtifact(f *deployFrame) error {
	// 1) 校验 + 算 digest。artifact.Decode 一次把**全部**问题列出来（"; " 连接），
	//    用户改一次就能把错改全，而不是改一条跑一次。
	art, digest, err := artifact.Decode(f.Artifact)
	if err != nil {
		a.log.Warn("deploy rejected: invalid artifact",
			"event", "agent.deploy.invalid_artifact",
			"deploy_id", f.DeployID, "error", err.Error())
		return a.replyApply(&applyFrame{
			DeployID: f.DeployID,
			State:    ws.ApplyStateFailed,
			Reason:   fmt.Sprintf("%s: %v", ws.ApplyReasonInvalidArtifact, err),
		})
	}

	reply := &applyFrame{
		DeployID:   f.DeployID,
		Capability: art.Metadata.Name,
		Version:    art.Metadata.Version,
		Digest:     digest,
	}

	// 2) 探依赖：产物声明 requires 的外部命令必须真的在这台机器上。
	//    部署期就失败，比等第一个任务跑到才炸好得多——控制台能直接告诉用户
	//    「这台没装 claude」。多缺几个用 ";" 连起来，一次说全。
	if missing := missingRequirements(art.Spec.Requires); len(missing) > 0 {
		reply.State = ws.ApplyStateFailed
		reply.Reason = strings.Join(missing, ";")
		a.log.Warn("deploy rejected: missing requirement",
			"event", "agent.deploy.missing_requirement",
			"deploy_id", f.DeployID,
			"capability", art.Metadata.Name, "version", art.Metadata.Version,
			"reason", reply.Reason)
		return a.replyApply(reply)
	}

	// 3) 原子落盘 + 更新索引（同一个 put：产物落好了索引才有意义）。
	if err := a.cache.put(art, digest); err != nil {
		reply.State = ws.ApplyStateFailed
		reply.Reason = fmt.Sprintf("%s: %v", ws.ApplyReasonWriteFailed, err)
		a.log.Warn("deploy failed: write artifact",
			"event", "agent.deploy.write_failed",
			"deploy_id", f.DeployID, "error", err.Error())
		return a.replyApply(reply)
	}

	// 4) 回报 applied，然后重发 hello 让中枢对账。
	reply.State = ws.ApplyStateApplied
	a.log.Info("artifact applied",
		"event", "agent.deploy.applied",
		"deploy_id", f.DeployID,
		"capability", art.Metadata.Name,
		"version", art.Metadata.Version,
		"digest", digest,
		"files", len(art.Spec.Run.Files),
	)
	if err := a.replyApply(reply); err != nil {
		return err
	}
	return a.sendHello()
}

// removeArtifact 撤销一份产物：摘索引 → 回 applied → 重发 hello。
//
// # remove 帧怎么定位能力
//
// DeployFrame 冻结时没有 capability/version 字段（契约 §4 只有 deploy_id /
// action / artifact），所以 remove **必须把 artifact 带下来**——中枢手里本来就有
// 这份产物（artifacts 表）。没有 artifact 就没法定位，只能回 invalid_artifact：
// 猜一个名字去删是更坏的选择（删错的代价是别人机器上一份能跑的东西消失）。
//
// 本地变体也一样能被 remove 掉：索引项指向的 digest 与产物目录里的内容可能已经
// 不一致（用户手改过），但 remove 处理的是「这台机器不再托管这个能力」，
// 与内容变没变无关。
func (a *nodeAgent) removeArtifact(f *deployFrame) error {
	art, _, err := artifact.Decode(f.Artifact)
	if err != nil {
		a.log.Warn("remove rejected: invalid artifact",
			"event", "agent.deploy.remove_invalid_artifact",
			"deploy_id", f.DeployID, "error", err.Error())
		return a.replyApply(&applyFrame{
			DeployID: f.DeployID,
			State:    ws.ApplyStateFailed,
			Reason: fmt.Sprintf("%s: remove 帧缺少可解析的 artifact: %v",
				ws.ApplyReasonInvalidArtifact, err),
		})
	}

	name, version := art.Metadata.Name, art.Metadata.Version
	// 先记下索引指向的 digest（摘掉之后就查不到了）：apply 帧回它便于审计
	// 「撤掉的到底是哪一份内容」。没缓存过就是空串（omitempty）。
	digest, _ := a.cache.readIndex(name, version)

	reply := &applyFrame{
		DeployID:   f.DeployID,
		Capability: name,
		Version:    version,
		Digest:     digest,
	}
	if err := a.cache.remove(name, version); err != nil {
		reply.State = ws.ApplyStateFailed
		reply.Reason = fmt.Sprintf("%s: %v", ws.ApplyReasonWriteFailed, err)
		a.log.Warn("remove failed",
			"event", "agent.deploy.remove_failed",
			"deploy_id", f.DeployID, "capability", name, "version", version, "error", err.Error())
		return a.replyApply(reply)
	}

	// 撤销成功也算 applied（契约 §4）：中枢的状态机只有 applied / failed 两个出口，
	// 「已经不在缓存里了」就是这次撤销想要的结果。
	reply.State = ws.ApplyStateApplied
	a.log.Info("artifact removed",
		"event", "agent.deploy.removed",
		"deploy_id", f.DeployID, "capability", name, "version", version, "digest", digest)
	if err := a.replyApply(reply); err != nil {
		return err
	}
	return a.sendHello()
}

// missingRequirements 返回缺失的外部命令，每项形如 "missing_requirement:claude"。
//
// 用 exec.LookPath（同 PATH 语义），与「脚本里 `claude ...` 能不能找到」完全一致：
// 探的是「这个命令在不在」，不是「装没装某个包」。
func missingRequirements(requires []string) []string {
	missing := make([]string, 0, len(requires))
	for _, cmd := range requires {
		if _, err := exec.LookPath(cmd); err != nil {
			missing = append(missing, ws.ApplyReasonMissingRequirement+":"+cmd)
		}
	}
	return missing
}

// replyApply 发一帧 apply（部署结果的唯一回报通道）。
func (a *nodeAgent) replyApply(f *applyFrame) error {
	f.Type = frameTypeApply
	if err := a.send(f); err != nil {
		return fmt.Errorf("send apply %s: %w", f.DeployID, err)
	}
	return nil
}

// sendHello 重发一帧 hello（部署 / 撤销之后的对账入口）。
//
// applied 每次都从 cache 现扫：缓存是唯一事实，内存里再存一份就会飘。
func (a *nodeAgent) sendHello() error {
	if err := a.send(a.cfg.hello(a.cache.applied())); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	return nil
}
