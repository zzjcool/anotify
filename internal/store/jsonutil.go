package store

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// 本文件是 store 里 JSON 语义比较的唯一实现（emit 幂等 / 事件去重都要它）。
//
// 为什么不用 bytes.Equal：同一份 data 经过一次 HTTP 往返可能被重排键、改空白
// （`{"a":1,"b":2}` vs `{"b":2,"a":1}`），字节比较会把「同一个请求的重发」
// 判成「不同请求」→ 409 emit_id_conflict，而调用方什么也没改。
//
// 为什么用 UseNumber：否则 1 与 1.0 都会变成 float64 而被判相等——把真实的
// 类型改动漏过去；UseNumber 保留原始写法，两者不等。

// sameJSON 比较两段 JSON 是否语义相同（键序、空白、数字写法都不影响）。
func sameJSON(a, b []byte) bool {
	va, err := decodeJSONValue(a)
	if err != nil {
		return false
	}
	vb, err := decodeJSONValue(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// decodeJSONValue 用 UseNumber 解一段 JSON（数字保留原始写法）。
func decodeJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
