package sourcespec

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// marshal 把值编成 RawMessage（本包所有输出的唯一出口）。
func marshal(v any) (json.RawMessage, error) {
	b, err := marshalCanonical(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// marshalCanonical 编 JSON：encoding/json 对 map 键排序，所以字节形态是确定的。
//
// 为什么不用一个自己的 canonical JSON 实现：本包只需要「同一份值 → 同一串字节」，
// 用来做落库与比较，不需要跨语言/跨版本的可复现性（那才要自造 canonicalizer）。
func marshalCanonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errf("无法编码 JSON：%v", err)
	}
	return b, nil
}

// canonicalEqual 比较两段 JSON 是否语义相同（键序、空白、数字写法都不影响）。
//
// 用 UseNumber 解码：否则 1 与 1.0 都会变成 float64 而被判相等——那会把
// 「用户把参数从整数改成了浮点」这种真实改动漏过去。
func canonicalEqual(a, b []byte) bool {
	va, err := decodeAny(a)
	if err != nil {
		return false
	}
	vb, err := decodeAny(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// decodeAny 用 UseNumber 解一段 JSON（数字保留原始写法）。
func decodeAny(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
