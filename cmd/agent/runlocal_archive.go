package main

// 本文件是 **standalone 执行的落档**（issue #320 §3）：`anotify-agent run` 在给了
// `--data-dir` 时，把这次执行（输入 + 完整输出 + meta）写进 `<data-dir>/runs/`，
// 走的是与中枢派单**同一个** `runStore.Save`、同一种目录形状：
//
//	<data-dir>/runs/<trace>/<capability>@<version>/<seq>-a<attempt>/
//
// 唯一不同的是 trace 的命名空间：本地执行没有中枢的 trace_id，于是用
// `local_<UTC 时间戳>` 造一个本地 id。三段仍然可寻址（`show <ref>` 能直接读），
// `runs --trace local_…` 也能按本地批次筛。
//
// # 为什么这件事值得做
//
// 此前全仓唯一的 runs.Save 在节点模式的 client.go 里，所以 `runs` 里永远看不到
// 「我在本机试跑的那一次」——最需要回看的那一次，恰恰不在档案里。超时退 124 之后
// 如果没有 ref 可指，「去 runs/show 看」就只是一句空话。
//
// # 边界（与 issue 一致）
//
//   - 没给 `--data-dir` 时**不落档**：standalone 的工作目录在系统临时目录，
//     落一份随时会被清掉的档案比不落更坏；但 stderr 必须明说（见 runStandalone）。
//   - 落档失败绝不改退出码：跑成功就是成功，磁盘满只该 Warn（与 client 侧同一条纪律）。

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

const (
	// localRunTaskID 是 standalone 执行的 task_id。
	//
	// 不是空串：脚本里 `"$ANOTIFY_TASK_ID"` 会变成空路径片段，比报错更难查。
	// 也不与中枢的 tsk_* 撞名（.tasks/local 是一条本地索引，不会盖掉任何中枢任务）。
	localRunTaskID = "local"

	// localTracePrefix 是本地 trace 命名空间的前缀（一眼看出这次不在中枢的 trace 里）。
	localTracePrefix = "local_"
)

// localTraceID 生成一次 standalone 执行的本地 trace id：`local_<UTC 时间戳>`。
//
// 时间戳带纳秒：同一秒内连跑两次（单测、脚本循环）也不会互相覆盖档案——
// 覆盖掉上一次的现场正是排障时最不想发生的事。格式仍是**单段**（runSegment
// 允许的字符集），可以直接当目录名。
func localTraceID(now time.Time) string {
	return localTracePrefix + now.UTC().Format("20060102T150405.000000000Z")
}

// localRunRecord 把一次 standalone 执行组装成要落盘的全部内容。
//
// 与 client.go 的 runRecordFor 是同一份形状、同一套口径（params 原样、两个流分开、
// 身份字段填全），只是数据来源不同：那边来自 assign 帧，这边来自命令行 + 产物。
// Attempt / SeqInTrace 固定 1：本地一次执行既不重试也不在 trace 里排队。
func localRunRecord(art *artifact.Artifact, params paramFlags, trace string, out outcome,
	started, finished time.Time,
) runRecord {
	in := runInput{
		TraceID:    trace,
		TaskID:     localRunTaskID,
		Capability: art.Metadata.Name,
		Version:    art.Metadata.Version,
		Attempt:    1,
		SeqInTrace: 1,
		// params 原样：人自己在 `-p` 里给的东西，没有脱敏的理由（档案只在本机）。
		Params:  map[string]any(params),
		WorkDir: out.WorkDir,
	}
	return runRecord{
		Input: in,
		Meta: runMeta{
			Status:   out.Status,
			ExitCode: out.ExitCode,
			Summary:  out.Summary,
			Error:    out.Err,

			StdoutBytes:     out.ArchiveStdoutBytes,
			StderrBytes:     out.ArchiveStderrBytes,
			StdoutTruncated: out.ArchiveStdoutTruncated,
			StderrTruncated: out.ArchiveStderrTruncated,
		},
		// 两份完整输出（CaptureFull 时才有内容；没开就落空文件，meta 如实说 0 字节）。
		Stdout: out.ArchiveStdout,
		Stderr: out.ArchiveStderr,

		Started:  started,
		Finished: finished,
	}
}

// archiveLocalRun 把一份 runRecord 落进 runs/，返回可给 `show` 用的三段 ref。
//
// 返回空串 = 没落成（调用方据此**不**打印一个指向不存在档案的 ref）。
// 失败只 Warn：这次执行的结果（退出码）与该不该落档无关。
func archiveLocalRun(store *runStore, rec runRecord, log *slog.Logger) string {
	if store == nil {
		return ""
	}
	rel, err := runDirRel(rec.Input)
	if err != nil {
		// 形状错只可能来自内部 bug（trace 是我们自己造的），但仍不 panic。
		log.Warn("standalone run archive path",
			"event", "agent.run.archive_failed", "error", err.Error())
		return ""
	}
	if _, err := store.Save(rec); err != nil {
		log.Warn("standalone run archive write failed",
			"event", "agent.run.archive_failed",
			"ref", rel,
			"error", err.Error())
		return ""
	}
	return rel
}

// writeLocalTimeoutHint 在超时（退 124）时给出「接下来去哪儿看」的那一行。
//
// 有档：直接给两条能粘的命令（runs 列表 + show 这一次）。
// 无档：明说没落档、为什么，以及下次怎么让它可回看——不能给一个不存在的 ref。
func writeLocalTimeoutHint(stderr io.Writer, dataDir, ref string) {
	if stderr == nil {
		return
	}
	if ref != "" {
		// 一行（issue §4 要求「追加一行」）：把两条命令用「、」串起来，粘贴时再拆。
		fmt.Fprintf(stderr,
			"anotify-agent: agent 判定超时（退 %d）；这次执行已落档："+
				"anotify-agent runs --data-dir %s 、 anotify-agent show %s --data-dir %s\n",
			exitTimeout, dataDir, ref, dataDir)
		return
	}
	fmt.Fprintf(stderr,
		"anotify-agent: agent 判定超时（退 %d）；本次未落档（没给 --data-dir），"+
			"加 --data-dir 后再跑就能用 `anotify-agent runs` / `show <ref>` 回看\n",
		exitTimeout)
}
