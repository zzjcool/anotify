package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 本文件是**中枢下发**的那条升级路径（CONTRACT-agent-upgrade §5）：
//
//	中枢推 upgrade 帧 → 节点自己去 GET /v1/node/binary → 校验 sha256 →
//	原子替换自己的二进制 → 回 upgrade_ack → 退出（systemd 拉起新版本）
//
// 与 CLI（upgrade.go 的 `anotify-agent upgrade`）的**区别只有触发方式**：下载、校验、
// 替换、错误码必须是同一套语义，否则「CLI 说 need_root、控制台说 download_failed」
// 这种事会让排障对着两份日志吵起来。所以这里复用同一批原子件（requestBinary /
// downloadTo / openDownloadFile / isPermissionError / shortSHA），只在失败时把
// 错误**翻译**成契约里的短码。
//
// # 为什么二进制不塞进帧里
//
// 帧上限 1 MiB（ws.NodeFrameSoftLimit），agent 是 15 MB 量级。一条 15 MB 的帧会
// 堵住同一条连接上的心跳与 assign —— 节点侧读超时 = 掉线，升级反而把节点弄没了。
// 帧里只有三件事：去哪拿、拿到后该是什么、目标版本叫什么。

// upgradeAckTimeout 是回 ack 的写超时。
//
// 单独给一个值而不是复用任务写超时：升级成功之后**进程马上要退出**，这一帧是
// 中枢得知「到底成没成」的唯一途径，多等一会儿比丢掉它划算。
const upgradeAckTimeout = 10 * time.Second

// upgradeResult 是一次下发升级的结论（可判别的短码 + 给人看的原因）。
type upgradeResult struct {
	OK      bool
	Version string
	Commit  string
	// Target 是替换的文件路径（成功时用于日志）。
	Target string
	// Code 是契约 §5 的短码（need_root / bad_sha / download_failed / not_supported）；
	// 成功时为空。
	Code   string
	Reason string
}

// handleUpgrade 处理一帧 upgrade。
//
// 顺序是有意的：
//  1. 先校验帧里的东西（URL 形状、sha 形状）——坏帧不下载，直接 bad_sha；
//  2. 再确定目标文件（自身可执行）；判不出来就 not_supported，别瞎写别处；
//  3. 下载 + 校验 + 原子替换（与 CLI 同一套）；
//  4. 先 ack 再退出。**失败绝不退出**：退出等于节点掉线，而用户正是需要看到
//     「为什么没升上去」（need_root 最常见，得让人去跑 sudo）。
func (a *nodeAgent) handleUpgrade(f *upgradeFrame) {
	res := a.applyUpgrade(f)

	ack := &upgradeAckFrame{
		Type:   frameTypeUpgradeAck,
		OK:     res.OK,
		Error:  res.Code,
		Commit: res.Commit,
	}
	if res.OK {
		ack.Version = res.Version
	}
	if err := a.send(ack); err != nil {
		// 连 ack 都发不出去：连接已经坏了，退出也没人知道。记一条就够。
		a.log.Warn("upgrade ack failed",
			"event", "agent.upgrade.ack_failed", "error", err.Error())
		return
	}
	if !res.OK {
		a.log.Warn("upgrade rejected",
			"event", "agent.upgrade.rejected", "code", res.Code, "error", res.Reason)
		return
	}
	a.log.Info("upgrade applied, exiting so the supervisor restarts the new binary",
		"event", "agent.upgrade.applied",
		"version", res.Version, "commit", res.Commit, "target", res.Target)
	// 退出让 systemd（Restart=always）拉起新版本。
	//
	// 为什么不在原地 exec 自己：exec 会保留旧进程的 cwd/环境/已打开的 socket，
	// 而 upgrade 恰好要求「全新的进程」（模块级状态、单例、socket 绑定都可能残留）。
	// 交给 supervisor 是最简单且可验证的路径；没有 supervisor 的部署（容器/前台跑）
	// 会在这里结束 —— 契约里写明的那条代价。
	a.exit(0)
}

// applyUpgrade 做真正的下载/校验/替换，返回可判别的结论（不退出进程，便于测试）。
func (a *nodeAgent) applyUpgrade(f *upgradeFrame) upgradeResult {
	sha := strings.ToLower(strings.TrimSpace(f.SHA256))
	if !isHexSHA256(sha) {
		return upgradeResult{Code: upgradeErrBadSHA,
			Reason: fmt.Sprintf("帧里的 sha256 形状不合法（%q），拒绝下载", f.SHA256)}
	}
	// 目标 URL 只允许中枢自己（帧里的 url_path 是相对的）。
	//
	// 为什么不信帧里的整条 URL：那条帧来自中枢，但把「去哪下载可执行文件」变成
	// 中枢可以任意填的字段，等于让中枢（或任何能推帧的人）指挥节点从任意地址拉
	// 一个二进制并以自身权限跑起来。这里只接受与配置里的 server **同源**的地址。
	endpoint, err := a.upgradeEndpoint(f.URLPath)
	if err != nil {
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: err.Error()}
	}
	target, err := resolveTarget(a.installToForTest, os.Executable, nil)
	if err != nil {
		return upgradeResult{Code: upgradeErrNotSupported, Reason: err.Error()}
	}

	resp, err := requestBinary(endpoint, a.cfg.Token)
	if err != nil {
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return upgradeResult{Code: upgradeErrDownloadFailed,
			Reason: fmt.Sprintf("GET %s 返回 %d（期望 200）", endpoint, resp.StatusCode)}
	}

	tmp, placed, err := openDownloadFile(target)
	if err != nil {
		if isPermissionError(err) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
			return upgradeResult{Code: upgradeErrNeedRoot,
				Reason: fmt.Sprintf("目标目录不可写：%s（%v）", filepath.Dir(target), err)}
		}
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: err.Error()}
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()

	got, err := downloadTo(tmp, resp.Body)
	if err != nil {
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: err.Error()}
	}
	if got != sha {
		// 契约：不符即丢，**绝不**装上去试。旧二进制一个字节都没动。
		return upgradeResult{Code: upgradeErrBadSHA,
			Reason: fmt.Sprintf("校验失败：下载内容 sha256 = %s，中枢声明 %s；已丢弃，%s 未改动", got, sha, target)}
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: fmt.Sprintf("chmod 0755: %v", err)}
	}
	if !placed {
		// 目录一开始就不可写：不静默改写到别处（那会「升级报成功、跑的却还是旧的」）。
		return upgradeResult{Code: upgradeErrNeedRoot,
			Reason: fmt.Sprintf("目标目录不可写：%s", filepath.Dir(target))}
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		if isPermissionError(err) {
			return upgradeResult{Code: upgradeErrNeedRoot,
				Reason: fmt.Sprintf("替换 %s 需要 root：%v", target, err)}
		}
		return upgradeResult{Code: upgradeErrDownloadFailed, Reason: fmt.Sprintf("替换 %s: %v", target, err)}
	}
	keep = true

	version := strings.TrimSpace(f.TargetVersion)
	if version == "" {
		version = "unknown"
	}
	return upgradeResult{OK: true, Version: version, Target: target}
}

// upgradeEndpoint 把帧里的相对路径拼成中枢上的绝对地址，并强制校验它没有跑到
// 别的 host 上（open redirect / 中间人被改帧时，节点不会去别处拉二进制）。
func (a *nodeAgent) upgradeEndpoint(urlPath string) (string, error) {
	p := strings.TrimSpace(urlPath)
	if p == "" {
		p = nodeBinaryPath // 帧里没给就按契约的固定路径
	}
	base, err := binaryEndpoint(a.cfg.Server)
	if err != nil {
		return "", err
	}
	root, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("解析中枢地址 %q: %w", base, err)
	}
	ref, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("解析 url_path %q: %w", p, err)
	}
	// 绝对 URL 只允许与中枢同源。
	if ref.IsAbs() {
		if ref.Scheme != root.Scheme || ref.Host != root.Host {
			return "", fmt.Errorf("url_path %q 与中枢 %s 不同源，拒绝从别处下载二进制", p, root.Host)
		}
		return ref.String(), nil
	}
	return root.ResolveReference(ref).String(), nil
}

// isHexSHA256 校验 64 位十六进制（不带 "sha256:" 前缀，契约 §5 的线形状）。
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
