package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestArtifactTransferConstants 钉 A1 上传通道的 wire 常量。
//
// 这些字符串是两侧共享契约：改一个字中枢/节点就对不上（401/404 或路径错）。
func TestArtifactTransferConstants(t *testing.T) {
	if NodeTaskArtifactsPathPrefix != "/v1/node/tasks/" ||
		NodeTaskArtifactsPathInfix != "/artifacts/" {
		t.Fatalf("上传路径漂了：%q + %q", NodeTaskArtifactsPathPrefix, NodeTaskArtifactsPathInfix)
	}
	if ArtifactTransferContentType != "application/octet-stream" {
		t.Fatalf("content type 必须是原始字节：%q", ArtifactTransferContentType)
	}
	wantHeaders := map[string]string{
		ArtifactHeaderAttempt:      "X-Anotify-Attempt",
		ArtifactHeaderLeaseID:      "X-Anotify-Lease-Id",
		ArtifactHeaderTaskToken:    "X-Anotify-Task-Token",
		ArtifactHeaderArtifactPath: "X-Anotify-Artifact-Path",
	}
	for got, want := range wantHeaders {
		if got != want {
			t.Fatalf("header 常量漂了：%q != %q", got, want)
		}
	}
	if MaxArtifactTransferBytes != 16<<20 {
		t.Fatalf("单文件上限必须是 16 MiB，实际 %d", MaxArtifactTransferBytes)
	}
	if MaxAttemptArtifactBytes != 64<<20 {
		t.Fatalf("单 attempt 上限必须是 64 MiB，实际 %d", MaxAttemptArtifactBytes)
	}
	if MaxArtifactTransferItems != 32 {
		t.Fatalf("单 attempt 条目上限必须是 32，实际 %d", MaxArtifactTransferItems)
	}
	if ArtifactUploadStatusCreated != 201 || ArtifactUploadStatusOK != 200 {
		t.Fatalf("上传成功状态必须是 201/200，实际 %d/%d",
			ArtifactUploadStatusCreated, ArtifactUploadStatusOK)
	}
	if strings.ContainsRune(ArtifactHeaderTaskToken, ':') {
		t.Fatal("header 名不能带冒号")
	}
}

// TestArtifactTransferErrorCodes 钉 A1 错误码与「不是任务 infra 码」的边界。
func TestArtifactTransferErrorCodes(t *testing.T) {
	codes := []string{
		ArtifactErrPathInvalid, ArtifactErrHashMismatch,
		ArtifactErrTooLarge, ArtifactErrConflict,
	}
	want := []string{
		"artifact_path_invalid", "artifact_hash_mismatch",
		"artifact_too_large", "artifact_conflict",
	}
	for i, got := range codes {
		if got != want[i] {
			t.Fatalf("A1 错误码漂了：%q != %q", got, want[i])
		}
		if IsFrameErrorCode(got) || IsPruneReason(got) || IsTaskRejectReason(got) {
			t.Fatalf("%q 不能混进冻结的帧/剪枝/reject 码集合", got)
		}
	}
	if ReasonArtifactUploadFailed != "artifact_upload_failed" {
		t.Fatalf("任务终态原因必须是 artifact_upload_failed，实际 %q", ReasonArtifactUploadFailed)
	}
	if IsRetryableInfra(ReasonArtifactUploadFailed) {
		t.Fatal("产物上传失败不是可自动重试的 infra 事实")
	}
}

// TestNodeArtifactUploadPath 钉路径拼法：taskId 转义、sha256 必须 64 位小写 hex。
func TestNodeArtifactUploadPath(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	got, err := NodeArtifactUploadPath("tsk_1", sha)
	if err != nil {
		t.Fatalf("NodeArtifactUploadPath: %v", err)
	}
	if want := "/v1/node/tasks/tsk_1/artifacts/" + sha; got != want {
		t.Fatalf("路径漂了：\n got %s\nwant %s", got, want)
	}

	escaped, err := NodeArtifactUploadPath("task/with space", sha)
	if err != nil {
		t.Fatalf("NodeArtifactUploadPath: %v", err)
	}
	if strings.Contains(escaped, " ") || strings.Contains(escaped, "task/with") {
		t.Fatalf("taskId 必须做路径转义：%s", escaped)
	}

	for _, bad := range []string{"", "sha256:" + sha, strings.ToUpper(sha), sha[:63], sha + "a"} {
		if _, err := NodeArtifactUploadPath("tsk_1", bad); err == nil {
			t.Fatalf("sha256 %q 应被拒绝", bad)
		}
	}
	if _, err := NodeArtifactUploadPath("", sha); err == nil {
		t.Fatal("空 taskId 应被拒绝")
	}
}

// TestValidArtifactSHA256 钉 64 位小写 hex（大小写不混用、不接受前缀）。
func TestValidArtifactSHA256(t *testing.T) {
	good := strings.Repeat("0123456789abcdef", 4)
	if !ValidArtifactSHA256(good) {
		t.Fatal("合法 sha256 被拒")
	}
	for _, bad := range []string{"", good[:63], good + "0", strings.ToUpper(good), "sha256:" + good, good[:63] + "g"} {
		if ValidArtifactSHA256(bad) {
			t.Fatalf("非法 sha256 被接受：%q", bad)
		}
	}
}

// TestValidArtifactLogicalPath 钉托管逻辑路径：规范相对路径，不能拼宿主路径。
func TestValidArtifactLogicalPath(t *testing.T) {
	good := []string{"artifacts/report.md", "artifacts/a/b/c.txt", "x"}
	for _, p := range good {
		if !ValidArtifactLogicalPath(p) {
			t.Fatalf("合法逻辑路径被拒：%q", p)
		}
	}
	bad := []string{
		"", "/abs/path", "../escape", "a/../b", "a//b", "a/./b",
		"a\\b", "a\x00b", "trailing/", "/", ".", "..",
		"a\r\nb", "a\rb", "a\nb", "a\tb", "a\x1fb", "a\x7fb",
		strings.Repeat("a", 256),
	}
	for _, p := range bad {
		if ValidArtifactLogicalPath(p) {
			t.Fatalf("非法逻辑路径被接受：%q", p)
		}
	}
}

// TestArtifactExportInvalidReason 钉 A2 的 artifact_export_invalid：确定性失败、
// 不是 infra 重试码、不混进帧/剪枝/reject 集合（只加这一个码，不广扩枚举）。
func TestArtifactExportInvalidReason(t *testing.T) {
	if ReasonArtifactExportInvalid != "artifact_export_invalid" {
		t.Fatalf("ReasonArtifactExportInvalid = %q", ReasonArtifactExportInvalid)
	}
	if IsRetryableInfra(ReasonArtifactExportInvalid) {
		t.Fatal("导出目录非法是确定性失败，不能算 infra 可重试")
	}
	if IsFrameErrorCode(ReasonArtifactExportInvalid) || IsPruneReason(ReasonArtifactExportInvalid) ||
		IsTaskRejectReason(ReasonArtifactExportInvalid) {
		t.Fatal("artifact_export_invalid 只属于任务失败码集合")
	}
}

// TestArtifactPathHeader 钉 header 编码：URL 转义 + 控制字符/注入一律拒绝。
func TestArtifactPathHeader(t *testing.T) {
	for _, p := range []string{"artifacts/report.md", "artifacts/a b/c.txt", "artifacts/中文.md"} {
		encoded, err := EncodeArtifactPathHeader(p)
		if err != nil {
			t.Fatalf("EncodeArtifactPathHeader(%q): %v", p, err)
		}
		if strings.ContainsAny(encoded, " \r\n") {
			t.Fatalf("header 值不能含空白/CR/LF：%q", encoded)
		}
		decoded, err := DecodeArtifactPathHeader(encoded)
		if err != nil {
			t.Fatalf("DecodeArtifactPathHeader(%q): %v", encoded, err)
		}
		if decoded != p {
			t.Fatalf("header 往返不一致：%q != %q", decoded, p)
		}
	}
	if _, err := EncodeArtifactPathHeader("a\r\nInjected: 1"); err == nil {
		t.Fatal("CRLF 逻辑路径必须被拒绝（防 header 注入）")
	}
	if _, err := DecodeArtifactPathHeader("a%0d%0aInjected%3a%201"); err == nil {
		t.Fatal("转义后的 CRLF 在解码后也必须被拒绝")
	}
	if _, err := DecodeArtifactPathHeader("%zz"); err == nil {
		t.Fatal("非法转义必须被拒绝")
	}
}

// TestArtifactUploadResponseWire 钉响应 DTO 的 wire 形状与严格解码。
func TestArtifactUploadResponseWire(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	resp := ArtifactUploadResponse{TaskID: "tsk_1", Attempt: 2, Path: "artifacts/report.md", SHA256: sha, Size: 12}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"taskId", "attempt", "path", "sha256", "size"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("响应缺少 wire 键 %q：%s", key, raw)
		}
	}
	decoded, err := DecodeArtifactUploadResponse(raw)
	if err != nil {
		t.Fatalf("DecodeArtifactUploadResponse: %v", err)
	}
	if *decoded != resp {
		t.Fatalf("响应往返不一致：%+v != %+v", *decoded, resp)
	}
}

// TestArtifactUploadResponseRejectsBadShape 钉严格解码：未知字段/错 attempt/超限都拒绝。
func TestArtifactUploadResponseRejectsBadShape(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	base := map[string]any{"taskId": "tsk_1", "attempt": 1, "path": "a", "sha256": sha, "size": 1}
	encode := func(m map[string]any) []byte {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return raw
	}
	if _, err := DecodeArtifactUploadResponse(encode(base)); err != nil {
		t.Fatalf("基线响应应通过：%v", err)
	}
	cases := map[string]map[string]any{
		"未知字段":      {"future": 1},
		"attempt 0": {"attempt": 0},
		"缺 taskId":  {"taskId": ""},
		"path 绝对":   {"path": "/etc/passwd"},
		"sha 大写":    {"sha256": strings.ToUpper(sha)},
		"size 超上限":  {"size": MaxArtifactTransferBytes + 1},
		"size 负数":   {"size": -1},
	}
	for name, patch := range cases {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range patch {
			if v == nil {
				delete(m, k)
				continue
			}
			m[k] = v
		}
		if _, err := DecodeArtifactUploadResponse(encode(m)); err == nil {
			t.Fatalf("%s 应被拒绝：%v", name, encode(m))
		}
	}
	if _, err := DecodeArtifactUploadResponse([]byte(`{"taskId":"t","attempt":1,"path":"a","sha256":"` + sha + `","size":1} trailing`)); err == nil {
		t.Fatal("响应之后的多余内容应被拒绝")
	}
}
