package source

// envelope.go 是 config.envelope 的**运行期**映射（#513 A1 / design #512）：把一次投递的
// （头 + 原始 body）翻译成 Ingest 的三元组。三个维度**各自**回落（缺省 = body.type /
// body.id / body.payload），于是「不写 envelope」与「写一份全默认」逐字等价。判据全部来自
// sourcespec（指针方言 / ScalarText / Scheme），这里只做「取哪个头、走哪条回落」。

import (
	"encoding/json"
	"strings"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// hookInput 把一次投递翻译成 Ingest 的输入；没有 envelope 就是今天的三段直读。
func hookInput(src *store.Source, body HookBody) (Input, error) {
	in := Input{
		Source:     src,
		Type:       strings.TrimSpace(body.Type),
		DeliveryID: strings.TrimSpace(body.ID),
		Payload:    body.Payload,
	}
	env := sourcespec.ParseConfig(src.Kind, src.Config).Envelope
	if env == nil {
		return in, nil
	}
	// envelope 配了就必须能看成对象（指针/whole 都以它为根；不是对象 → 400）。
	obj, ok := route.DecodePayload(body.Raw)
	if !ok {
		return Input{}, ErrBadPayload
	}
	in.Type = envelopeField(env.Type, body, obj, in.Type)
	in.DeliveryID = envelopeField(env.DeliveryID, body, obj, in.DeliveryID)
	switch {
	case env.Payload == nil:
		// 该维缺省：payload 仍取 body.payload（in.Payload 已经是它）。
	case env.Payload.Whole:
		in.Payload = body.Raw
	default:
		value, found := sourcespec.LookupPointer(obj, env.Payload.Pointer)
		object, isObject := value.(map[string]any)
		if !found || !isObject {
			// 不静默当 `{}`：坏指针必须当场可见（否则只会在下游长成一条 event_schema）。
			return Input{}, ErrBadPayload
		}
		raw, err := json.Marshal(object)
		if err != nil {
			return Input{}, ErrBadPayload
		}
		in.Payload = raw
	}
	return in, nil
}

// envelopeField 解析 type / deliveryId 一维：头值或指针，都为空时回落 fallback。joinFrom
// 只在**两段都非空**时拼接：半截 type（`issues.` / `.opened`）会让 match.types 永不命中。
func envelopeField(field *sourcespec.FieldSource, body HookBody, obj map[string]any, fallback string) string {
	if field == nil {
		return fallback
	}
	value := ""
	if field.Header != "" {
		value = strings.TrimSpace(body.header(field.Header))
	} else if raw, found := sourcespec.LookupPointer(obj, field.Pointer); found {
		value, _ = sourcespec.ScalarText(raw) // 对象/数组当 type 是写错了：按取不到处理
	}
	if joined := joinField(obj, field.JoinFrom); joined != "" && value != "" {
		value += "." + joined
	}
	return value
}

// joinField 取 joinFrom 的标量（取不到 / 非标量 / 空 → 空串 = 不参与拼接）。
func joinField(obj map[string]any, pointer string) string {
	if pointer == "" {
		return ""
	}
	raw, found := sourcespec.LookupPointer(obj, pointer)
	if !found {
		return ""
	}
	text, _ := sourcespec.ScalarText(raw)
	return text
}
