package protocol

// emit_test.go 是 E1-D2 共享 DTO 的 wire-presence 直接验收：
//
//   - 未声明版本（0）不公开哨兵值：`version` 字段省略（未知就是未知）；
//   - 已解析 / 明确声明的正版本恒返回；
//   - taskId 的语义不变：剪枝显式 `null`，已建任务（含派发失败 Task）是真实 id 字符串，
//     anode CLI 的成功判据（taskId 非 null 即接受）据此成立。

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEmitResponseVersionPresenceFollowsFacts(t *testing.T) {
	// ① 未声明且未解析：没有版本事实 → 不写 version（不公开哨兵 0）。
	unknown, err := json.Marshal(EmitResponse{To: "ghost"})
	if err != nil {
		t.Fatalf("marshal unknown: %v", err)
	}
	if bytes.Contains(unknown, []byte(`"version"`)) {
		t.Fatalf("未声明的版本不得公开: %s", unknown)
	}
	// taskId 的 null 语义不变（剪枝/未知身份都显式 null，不是省略）。
	if !bytes.Contains(unknown, []byte(`"taskId":null`)) {
		t.Fatalf("taskId 必须显式出现在回执里: %s", unknown)
	}

	// ② 明确声明的正版本恒返回（字段不能被 omitempty 吞掉）。
	taskID := "tsk_real"
	declared, err := json.Marshal(EmitResponse{TaskID: &taskID, EventID: "evt_1", To: "ghost@2", Version: 2})
	if err != nil {
		t.Fatalf("marshal declared: %v", err)
	}
	if !bytes.Contains(declared, []byte(`"version":2`)) {
		t.Fatalf("声明的正版本必须返回: %s", declared)
	}
	if !bytes.Contains(declared, []byte(`"taskId":"tsk_real"`)) {
		t.Fatalf("已建任务（含派发失败 Task）必须回真实 taskId: %s", declared)
	}

	// ③ 旧/未知回执（没有 version 字段）解码成 0，不报错、不猜版本。
	var legacy EmitResponse
	if err := json.Unmarshal([]byte(`{"taskId":null,"eventId":"evt_1","to":"ghost","pruned":{"reason":"param_missing"}}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.Version != 0 || legacy.TaskID != nil || legacy.Pruned == nil {
		t.Fatalf("legacy 回执解码 = %+v", legacy)
	}

	// ④ 正版本 roundtrip：字段恒在、值不漂移；CLI 成功判据（taskId 非 null）兼容。
	raw, err := json.Marshal(EmitResponse{TaskID: &taskID, To: "fixer", Version: 3})
	if err != nil {
		t.Fatalf("marshal roundtrip: %v", err)
	}
	var back EmitResponse
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal roundtrip: %v", err)
	}
	if back.Version != 3 || back.TaskID == nil || *back.TaskID != taskID {
		t.Fatalf("roundtrip = %+v", back)
	}
}
