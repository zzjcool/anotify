package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/protocol"
)

// L1-R3/R4 的节点侧行为：真实索引记录 + raw 尾部窗口，先 since/tail 再帧上限。

type logFeed struct {
	stream string
	data   []byte
}

// writeIndexed 造一份真实档案：raw 逐字节写盘，同时把同一批字节按 feed 顺序喂给
// 结构化索引（序号/at 由 now 决定）。raw 与索引都通过真实 writer 落盘。
func writeIndexed(t *testing.T, s *resultstore.Store, taskID string, attempt int,
	now func() time.Time, feeds ...logFeed) *resultstore.Attempt {
	t.Helper()
	rec, err := s.Begin(resultstore.Record{
		TaskID: taskID, Node: "node-a", Attempt: attempt, Capability: "echo",
		Digest: "sha256:aa", TTL: 0, Phase: "succeeded",
		CreatedAt: time.Now().UTC(), Manifest: []resultstore.Manifest{},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	idx := rec.OpenLogIndex(now)
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	raw := map[string][]byte{}
	for _, feed := range feeds {
		raw[feed.stream] = append(raw[feed.stream], feed.data...)
		if _, err := idx.Writer(feed.stream).Write(feed.data); err != nil {
			t.Fatalf("索引 writer 不能返回错误：%v", err)
		}
	}
	for _, stream := range []string{protocol.LogStreamStdout, protocol.LogStreamStderr} {
		name := resultstore.StdoutFileName
		if stream == protocol.LogStreamStderr {
			name = resultstore.StderrFileName
		}
		f, err := rec.OpenLogFile(name)
		if err != nil {
			t.Fatalf("OpenLogFile: %v", err)
		}
		if _, err := f.Write(raw[stream]); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		idx.CloseStream(stream)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("关闭索引: %v", err)
	}
	return rec
}

func requestLogs(t *testing.T, agent *Agent, f *protocol.LogsRequestFrame) *protocol.LogsResponseFrame {
	t.Helper()
	resp := agent.HandleLogsRequest(context.Background(), f)
	if resp == nil {
		t.Fatal("HandleLogsRequest 返回 nil")
	}
	return resp
}

func encodeSize(t *testing.T, resp *protocol.LogsResponseFrame) int {
	t.Helper()
	raw, err := protocol.Encode(resp)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return len(raw)
}

// TestLogsRequestIllegalArgumentsRejected 钉 R3：非法参数回 closed-set 错误，
// 不归一化成「0 = 最新 / 0 = 不限」，也不伪造 attempt/lines。
func TestLogsRequestIllegalArgumentsRejected(t *testing.T) {
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, nil, logFeed{protocol.LogStreamStdout, []byte("x\n")})
	agent := &Agent{Results: store}
	cases := []*protocol.LogsRequestFrame{
		{Type: protocol.FrameLogsRequest, RequestID: "r", Attempt: 1},
		{Type: protocol.FrameLogsRequest, TaskID: "t1", Attempt: 1},
		{Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r", Attempt: -1},
		{Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r", TailLines: -2},
		{Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r", TailLines: protocol.LogsTailLinesMax + 1},
		{Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r", SinceSeconds: -1},
	}
	for i, f := range cases {
		resp := requestLogs(t, agent, f)
		if resp.Error != protocol.LogsErrIllegalRequest {
			t.Fatalf("非法请求 #%d 必须 illegal_log_request：%+v", i, resp)
		}
		if resp.Attempt != 0 || resp.LogLines() != nil {
			t.Fatalf("非法请求不能伪造 attempt/lines：%+v", resp)
		}
		if raw, err := protocol.Encode(resp); err != nil || bytes.Contains(raw, []byte(`"lines"`)) {
			t.Fatalf("非法请求的 wire 不能带成功记录：%s err=%v", raw, err)
		}
	}
}

// TestLogsRequestNoArchiveRawOnlyAndUnreadable 区分三种事实：
// no_local_archive（没档案）、logs_unreadable（raw-only 旧档/坏索引）、成功。
func TestLogsRequestNoArchiveRawOnlyAndUnreadable(t *testing.T) {
	store := testStore(t)
	agent := &Agent{Results: store}

	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "missing", RequestID: "r0",
	})
	if resp.Error != protocol.LogsErrNoLocalArchive || resp.Attempt != 0 || resp.LogLines() != nil {
		t.Fatalf("无档案必须 no_local_archive：%+v", resp)
	}

	// 旧开发期 raw-only 档案：不迁移、不补时间，明确不可结构化读取。
	rec, err := store.Begin(resultstore.Record{
		TaskID: "raw-only", Node: "node-a", Attempt: 1, Capability: "echo",
		Digest: "sha256:aa", TTL: 0, Phase: "succeeded",
		CreatedAt: time.Now().UTC(), Manifest: []resultstore.Manifest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := rec.OpenLogFile(resultstore.StdoutFileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("legacy\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	resp = requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "raw-only", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != protocol.LogsErrUnreadable || resp.Attempt != 0 || resp.LogLines() != nil {
		t.Fatalf("raw-only 档案必须 logs_unreadable（不是空成功）：%+v", resp)
	}

	// 坏索引同样是 logs_unreadable。
	writeIndexed(t, store, "broken", 1, nil, logFeed{protocol.LogStreamStdout, []byte("x\n")})
	indexPath := filepath.Join(store.Root, hexNameForOps("broken"), "1", resultstore.LogsDirName, resultstore.LogIndexFileName)
	if err := os.WriteFile(indexPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "broken", RequestID: "r2", Attempt: 1,
	})
	if resp.Error != protocol.LogsErrUnreadable || resp.LogLines() != nil {
		t.Fatalf("坏索引必须 logs_unreadable：%+v", resp)
	}
}

// TestLogsResponseRealRecordsRawAndBytes 钉成功响应的完整形状：真实序号/stream/text/at，
// raw 逐字节尾部，bytes 只是回包 raw 内容字节数。
func TestLogsResponseRealRecordsRawAndBytes(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, steppingNow(start, time.Second),
		logFeed{protocol.LogStreamStdout, []byte("out-1\nout-2\n")},
		logFeed{protocol.LogStreamStderr, []byte("err-1\n")},
	)
	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" || resp.Attempt != 1 || resp.Truncated {
		t.Fatalf("小档案必须成功且不截断：%+v", resp)
	}
	if resp.Stdout != "out-1\nout-2\n" || resp.Stderr != "err-1\n" {
		t.Fatalf("raw 必须逐字节返回：%q / %q", resp.Stdout, resp.Stderr)
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须是回包 raw 字节数：%d", resp.Bytes)
	}
	lines := resp.LogLines()
	if len(lines) != 3 {
		t.Fatalf("应有三条记录（按喂入顺序）：%+v", lines)
	}
	want := []struct {
		seq    int
		stream string
		text   string
	}{
		{1, protocol.LogStreamStdout, "out-1"},
		{2, protocol.LogStreamStdout, "out-2"},
		{3, protocol.LogStreamStderr, "err-1"},
	}
	for i, w := range want {
		if lines[i].Sequence != w.seq || lines[i].Stream != w.stream || lines[i].Text != w.text {
			t.Fatalf("第 %d 条记录不对：%+v", i, lines[i])
		}
		if at, err := time.Parse(time.RFC3339Nano, lines[i].At); err != nil || at.IsZero() {
			t.Fatalf("at 必须是真实可解析时间：%q", lines[i].At)
		}
	}
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("编码后必须 ≤ 1 MiB：%d", size)
	}
}

func steppingNow(start time.Time, step time.Duration) func() time.Time {
	cur := start
	return func() time.Time {
		out := cur
		cur = cur.Add(step)
		return out
	}
}

func hexNameForOps(taskID string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(taskID); i++ {
		b.WriteByte(digits[taskID[i]>>4])
		b.WriteByte(digits[taskID[i]&0x0f])
	}
	return b.String()
}

func shaHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestLogsResponseBothStreamRealTailsWithinBudget 钉 R9-C：预算充足时两流各自
// 真实尾部都要在包内（同预算、真后缀、不是伪造的后缀字节），档案 SHA 不变。
func TestLogsResponseBothStreamRealTailsWithinBudget(t *testing.T) {
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, nil) // 合法零记录索引
	logsDir := filepath.Join(store.Root, hexNameForOps("t1"), "1", resultstore.LogsDirName)
	rawOut := append(bytes.Repeat([]byte("o"), 800<<10), []byte("\nSTDOUT-TAIL-UNIQUE-9f3a\n")...)
	rawErr := append(bytes.Repeat([]byte("e"), 700<<10), []byte("\nSTDERR-TAIL-UNIQUE-7c1b\n")...)
	if err := os.WriteFile(filepath.Join(logsDir, resultstore.StdoutFileName), rawOut, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, resultstore.StderrFileName), rawErr, 0o600); err != nil {
		t.Fatal(err)
	}
	sumOut, sumErr := shaHex(rawOut), shaHex(rawErr)

	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" || !resp.Truncated {
		t.Fatalf("超预算必须成功 + truncated：%+v", resp)
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须是回包 raw 字节数：%d", resp.Bytes)
	}
	if len(resp.Stdout) == 0 || len(resp.Stdout) != len(resp.Stderr) {
		t.Fatalf("预算充足时两流应拿到相同预算的真实尾部：stdout=%d stderr=%d", len(resp.Stdout), len(resp.Stderr))
	}
	if !bytes.Equal([]byte(resp.Stdout), rawOut[len(rawOut)-len(resp.Stdout):]) {
		t.Fatal("stdout 必须是原档案的真实尾部字节")
	}
	if !bytes.Equal([]byte(resp.Stderr), rawErr[len(rawErr)-len(resp.Stderr):]) {
		t.Fatal("stderr 必须是原档案的真实尾部字节")
	}
	if !strings.HasSuffix(resp.Stdout, "STDOUT-TAIL-UNIQUE-9f3a\n") ||
		!strings.HasSuffix(resp.Stderr, "STDERR-TAIL-UNIQUE-7c1b\n") {
		t.Fatalf("两流的真实尾标记都必须保留：%q / %q",
			resp.Stdout[max(0, len(resp.Stdout)-40):], resp.Stderr[max(0, len(resp.Stderr)-40):])
	}
	if lines := resp.LogLines(); lines == nil || len(lines) != 0 {
		t.Fatalf("零记录索引成功空必须是 []：%#v", lines)
	}
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("编码后必须 ≤ 1 MiB：%d", size)
	}
	if after, err := os.ReadFile(filepath.Join(logsDir, resultstore.StdoutFileName)); err != nil || shaHex(after) != sumOut {
		t.Fatalf("stdout 档案 SHA 不能因回包而被改变：%v", err)
	}
	if after, err := os.ReadFile(filepath.Join(logsDir, resultstore.StderrFileName)); err != nil || shaHex(after) != sumErr {
		t.Fatalf("stderr 档案 SHA 不能因回包而被改变：%v", err)
	}
}

// TestLogsResponseRecordsFirstInsufficientBudgetKeepsRawIntact 钉 R9-C：预算不足时
// 记录优先（最新完整 JSON 记录 ≤1MiB），raw 可以空；任何 raw 都必须是真实尾部，
// bytes 仍是回包 raw 长度，原档案 SHA/字节不变。
func TestLogsResponseRecordsFirstInsufficientBudgetKeepsRawIntact(t *testing.T) {
	store := testStore(t)
	const total, lineBytes = 300, 4096
	var feeds []logFeed
	for i := 0; i < total; i++ {
		text := fmt.Sprintf("%04d", i) + strings.Repeat("z", lineBytes-4) + "\n"
		feeds = append(feeds, logFeed{protocol.LogStreamStdout, []byte(text)})
	}
	writeIndexed(t, store, "t1", 1, nil, feeds...)

	logsDir := filepath.Join(store.Root, hexNameForOps("t1"), "1", resultstore.LogsDirName)
	rawOut := append(bytes.Repeat([]byte("o"), 2<<20), []byte("\nOUT-RAW-END-3d7e\n")...)
	rawErr := append(bytes.Repeat([]byte("e"), 2<<20), []byte("\nERR-RAW-END-6b2f\n")...)
	if err := os.WriteFile(filepath.Join(logsDir, resultstore.StdoutFileName), rawOut, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, resultstore.StderrFileName), rawErr, 0o600); err != nil {
		t.Fatal(err)
	}
	sumOut, sumErr := shaHex(rawOut), shaHex(rawErr)

	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" || !resp.Truncated {
		t.Fatalf("记录超预算必须成功 + truncated：%+v", resp)
	}
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("编码后必须 ≤ 1 MiB：%d", size)
	}
	lines := resp.LogLines()
	if len(lines) == 0 || len(lines) >= total {
		t.Fatalf("必须保留部分最新完整记录：%d", len(lines))
	}
	if !strings.HasPrefix(lines[len(lines)-1].Text, fmt.Sprintf("%04d", total-1)) {
		t.Fatalf("records 优先：最新记录必须是档案最后一条：%q", lines[len(lines)-1].Text[:8])
	}
	if first, last := lines[0].Sequence, lines[len(lines)-1].Sequence; first+len(lines)-1 != last {
		t.Fatalf("保留的必须是连续后缀：%d..%d 共 %d 条", first, last, len(lines))
	}
	for _, line := range lines {
		if len(line.Text) != lineBytes {
			t.Fatalf("记录不能被切半：%d", len(line.Text))
		}
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须等于回包 raw 字节数：%d", resp.Bytes)
	}
	if resp.Stdout != "" && !bytes.Equal([]byte(resp.Stdout), rawOut[len(rawOut)-len(resp.Stdout):]) {
		t.Fatal("raw 非空时必须是原档案真实尾部")
	}
	if resp.Stderr != "" && !bytes.Equal([]byte(resp.Stderr), rawErr[len(rawErr)-len(resp.Stderr):]) {
		t.Fatal("raw 非空时必须是原档案真实尾部")
	}
	if after, err := os.ReadFile(filepath.Join(logsDir, resultstore.StdoutFileName)); err != nil || shaHex(after) != sumOut {
		t.Fatalf("stdout 档案必须完整（SHA 不变）：%v", err)
	}
	if after, err := os.ReadFile(filepath.Join(logsDir, resultstore.StderrFileName)); err != nil || shaHex(after) != sumErr {
		t.Fatalf("stderr 档案必须完整（SHA 不变）：%v", err)
	}
}

// TestLogsResponseHugeSinceSecondsDoesNotReturnEmpty 钉 R9-C：协议允许的极大
// sinceSeconds 由节点安全饱和成「不限」，不能因 int64 乘法回绕成未来阈值而误空。
func TestLogsResponseHugeSinceSecondsDoesNotReturnEmpty(t *testing.T) {
	store := testStore(t)
	start := time.Now().Add(-time.Minute).UTC()
	writeIndexed(t, store, "t1", 1, steppingNow(start, time.Second),
		logFeed{protocol.LogStreamStdout, []byte("a\n")},
		logFeed{protocol.LogStreamStdout, []byte("b\n")},
	)
	agent := &Agent{Results: store}
	maxInt := int(^uint(0) >> 1)
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
		SinceSeconds: maxInt,
	})
	if resp.Error != "" || resp.Truncated {
		t.Fatalf("极大 sinceSeconds 必须成功且不截断：%+v", resp)
	}
	lines := resp.LogLines()
	if len(lines) != 2 || lines[0].Text != "a" || lines[1].Text != "b" {
		t.Fatalf("极大 sinceSeconds 饱和成不限，不能误返回空：%+v", lines)
	}
}

// TestLogsResponseSinceAndTailWindow 钉 R4 顺序：节点按**自己的当前时间**算 since
// 阈值 → 过滤 → tail 取最新；无匹配是合法空；raw 仍按既有能力返回。
func TestLogsResponseSinceAndTailWindow(t *testing.T) {
	start := time.Now().Add(-50 * time.Second).UTC()
	store := testStore(t)
	var feeds []logFeed
	for i := 0; i < 6; i++ {
		feeds = append(feeds, logFeed{protocol.LogStreamStdout, []byte{byte('a' + i), '\n'}})
	}
	writeIndexed(t, store, "t1", 1, steppingNow(start, 10*time.Second), feeds...)
	agent := &Agent{Results: store}

	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1",
		Attempt: 1, SinceSeconds: 25, TailLines: 2,
	})
	if resp.Error != "" || !resp.Truncated {
		t.Fatalf("tail 丢掉匹配记录必须 truncated：%+v", resp)
	}
	lines := resp.LogLines()
	if len(lines) != 2 || lines[0].Text != "e" || lines[1].Text != "f" {
		t.Fatalf("since→tail 必须取最新两条：%+v", lines)
	}
	if lines[0].Sequence != 5 || lines[1].Sequence != 6 {
		t.Fatalf("序号不能重编号：%+v", lines)
	}
	if resp.Stdout != "a\nb\nc\nd\ne\nf\n" {
		t.Fatalf("since 只过滤记录，raw 仍按既有能力返回：%q", resp.Stdout)
	}

	// 时间窗过滤掉全部记录：合法空数组、不是 truncated、也不是不可读。
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeIndexed(t, store, "old", 1, steppingNow(old, time.Second),
		logFeed{protocol.LogStreamStdout, []byte("ancient\n")})
	resp = requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "old", RequestID: "r2", Attempt: 1, SinceSeconds: 60,
	})
	if resp.Error != "" || resp.Truncated {
		t.Fatalf("无匹配记录必须成功空且不截断：%+v", resp)
	}
	if lines := resp.LogLines(); lines == nil || len(lines) != 0 {
		t.Fatalf("成功空必须是 []. 实际 %#v", lines)
	}
}

// TestLogsResponseFrameLimitKeepsNewestCompleteRecords 钉 R4 帧上限：先过滤/tail，
// 再取能完整编码的最新记录；记录不被切半，frame 精确 ≤ 1 MiB。
func TestLogsResponseFrameLimitKeepsNewestCompleteRecords(t *testing.T) {
	store := testStore(t)
	var feeds []logFeed
	const total = 400
	const lineBytes = 4096
	for i := 0; i < total; i++ {
		text := fmt.Sprintf("%04d", i) + strings.Repeat("z", lineBytes-4) + "\n"
		feeds = append(feeds, logFeed{protocol.LogStreamStdout, []byte(text)})
	}
	writeIndexed(t, store, "t1", 1, nil, feeds...)
	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" || !resp.Truncated {
		t.Fatalf("记录超帧必须成功 + truncated：%+v", resp)
	}
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("编码后必须 ≤ 1 MiB：%d", size)
	}
	lines := resp.LogLines()
	if len(lines) == 0 || len(lines) >= total {
		t.Fatalf("必须保留部分最新完整记录：%d", len(lines))
	}
	if got := lines[len(lines)-1].Text; !strings.HasPrefix(got, fmt.Sprintf("%04d", total-1)) {
		t.Fatalf("最新记录必须是最后一条：%q", got[:8])
	}
	for _, line := range lines {
		if len(line.Text) != lineBytes {
			t.Fatalf("记录不能被切半：%d", len(line.Text))
		}
	}
	if first, last := lines[0].Sequence, lines[len(lines)-1].Sequence; first+len(lines)-1 != last {
		t.Fatalf("保留的必须是连续后缀：%d..%d 共 %d 条", first, last, len(lines))
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须等于回包 raw 字节数：%d", resp.Bytes)
	}
}

// TestLogsResponseEscapedTextRespectsExactJSONLimit 钉「按编码后字节量」而不是按
// text 长度估算：6 倍 JSON 转义（控制字符）也必须精确 ≤ 1 MiB，且记录完整。
func TestLogsResponseEscapedTextRespectsExactJSONLimit(t *testing.T) {
	store := testStore(t)
	text := strings.Repeat("\x01", protocol.LogLineTextMaxBytes) + "\n"
	var feeds []logFeed
	for i := 0; i < 8; i++ {
		feeds = append(feeds, logFeed{protocol.LogStreamStdout, []byte(text)})
	}
	writeIndexed(t, store, "t1", 1, nil, feeds...)
	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("转义后编码必须 ≤ 1 MiB：%d", size)
	}
	if !resp.Truncated {
		t.Fatal("装不下全部记录必须 truncated")
	}
	lines := resp.LogLines()
	if len(lines) == 0 {
		t.Fatal("至少要保留一条能完整编码的最新记录")
	}
	for _, line := range lines {
		if line.Text != strings.Repeat("\x01", protocol.LogLineTextMaxBytes) {
			t.Fatalf("转义开销下仍必须是完整记录：%d", len(line.Text))
		}
	}
}

// TestLogsResponseEmptyArchiveIsValidEmpty 钉 R2：成功空必须来自真实存在的
// 零记录索引，而不是缺失/损坏档案。
func TestLogsResponseEmptyArchiveIsValidEmpty(t *testing.T) {
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, nil)
	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" || resp.Attempt != 1 || resp.Truncated || resp.Bytes != 0 {
		t.Fatalf("零输出档案必须成功空：%+v", resp)
	}
	if lines := resp.LogLines(); lines == nil || len(lines) != 0 {
		t.Fatalf("成功空必须是 []. 实际 %#v", lines)
	}
	if raw, err := protocol.Encode(resp); err != nil || !bytes.Contains(raw, []byte(`"lines":[]`)) {
		t.Fatalf("wire 上必须是 lines: []：%s err=%v", raw, err)
	}
}

// TestLogsResponseExplicitAttemptAndLatestIsolation 钉显式 attempt 与 latest 不串档。
func TestLogsResponseExplicitAttemptAndLatestIsolation(t *testing.T) {
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, nil, logFeed{protocol.LogStreamStdout, []byte("attempt-one\n")})
	writeIndexed(t, store, "t1", 2, nil, logFeed{protocol.LogStreamStdout, []byte("attempt-two\n")})
	agent := &Agent{Results: store}

	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Attempt != 1 || len(resp.LogLines()) != 1 || resp.LogLines()[0].Text != "attempt-one" {
		t.Fatalf("显式 attempt=1 不能越档：%+v", resp)
	}
	resp = requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r2",
	})
	if resp.Attempt != 2 || len(resp.LogLines()) != 1 || resp.LogLines()[0].Text != "attempt-two" {
		t.Fatalf("省略 attempt = 最近一次有档案的 attempt：%+v", resp)
	}
	resp = requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r3", Attempt: 9,
	})
	if resp.Error != protocol.LogsErrNoLocalArchive || resp.LogLines() != nil {
		t.Fatalf("不存在的 attempt 必须 no_local_archive：%+v", resp)
	}
}

// TestLogsResponseRawMoreIsNotSilentlyDropped 钉 >8 MiB raw 的更多标记仍传播为
// truncated（记录为空也不伪称「没有更多」）。
func TestLogsResponseRawMoreIsNotSilentlyDropped(t *testing.T) {
	store := testStore(t)
	writeIndexed(t, store, "t1", 1, nil) // 合法零记录索引
	rawPath := filepath.Join(store.Root, hexNameForOps("t1"), "1", resultstore.LogsDirName, resultstore.StdoutFileName)
	if err := os.WriteFile(rawPath, bytes.Repeat([]byte("r"), 9<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Results: store}
	resp := requestLogs(t, agent, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "r1", Attempt: 1,
	})
	if resp.Error != "" {
		t.Fatalf("索引有效就不是 unreadable：%+v", resp)
	}
	if lines := resp.LogLines(); lines == nil || len(lines) != 0 {
		t.Fatalf("零记录索引成功空必须是 []：%#v", lines)
	}
	if !resp.Truncated {
		t.Fatal("raw 超过读取窗口必须传播 truncated")
	}
	if size := encodeSize(t, resp); size > protocol.LogsResponseMaxBytes {
		t.Fatalf("编码后必须 ≤ 1 MiB：%d", size)
	}
	if resp.Bytes != int64(len(resp.Stdout)+len(resp.Stderr)) {
		t.Fatalf("bytes 必须等于回包 raw 字节数：%d", resp.Bytes)
	}
}
