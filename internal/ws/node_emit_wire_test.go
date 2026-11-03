package ws

import (
	"encoding/json"
	"testing"
)

// TestEmitFrameCarriesNoteAndSeq 是线格式的往返钉子。
//
// 为什么值得单独测：note 曾经在 schema.sql 的 emissions 建表里被漏掉，中枢收到后
// 静默丢弃、trace 里恒为空——而帧层与库层都不报错。把「帧能带、解码能回」钉住，
// 至少让「字段根本没定义」这种问题在编译/测试期暴露。
func TestEmitFrameCarriesNoteAndSeq(t *testing.T) {
	frame := &EmitFrame{
		Type: "emit", TaskID: "tsk_1", Seq: 3, Port: "fix",
		Data: json.RawMessage(`{"a":1}`), Note: "第 2 项",
	}
	data, err := Encode(frame)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeUpstream(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	back, ok := got.(*EmitFrame)
	if !ok {
		t.Fatalf("decode 类型 = %T, want *EmitFrame", got)
	}
	if back.Note != "第 2 项" || back.Seq != 3 || back.Port != "fix" || back.TaskID != "tsk_1" {
		t.Fatalf("emit 帧往返丢字段：%+v", back)
	}
	if string(back.Data) != `{"a":1}` {
		t.Fatalf("data 没原样带回：%s", back.Data)
	}
}

// TestEmitAckFrameShape 钉住 emit_ack 的线上形状（节点与中枢各写一半，形状漂移
// 只会在真链路上才暴露，所以在这里用字节级断言）。
func TestEmitAckFrameShape(t *testing.T) {
	data, err := Encode(&EmitAckFrame{Type: NodeFrameEmitAck, TaskID: "tsk_1", Seq: 2, Routed: 3, Pruned: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"type":"emit_ack","task_id":"tsk_1","seq":2,"routed":3,"pruned":1}`
	if string(data) != want {
		t.Fatalf("emit_ack 形状 = %s, want %s", data, want)
	}
	if !IsUpstream(NodeFrameEmit) || !IsDownstream(NodeFrameEmitAck) {
		t.Fatal("emit / emit_ack 的方向判定不对")
	}
}
