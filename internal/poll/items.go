package poll

import (
	"encoding/json"
	"fmt"

	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// inputFor 把一条 item 翻成一次标准入站：三个指针**在 item 内部**取值，缺省与 #515 同义
// （type → 源名、deliveryId → 不去重、payload → 整个 item）。
func inputFor(src *store.Source, cfg sourcespec.Config, item any) (source.Input, error) {
	in := source.Input{Source: src, Type: src.Name}
	env := cfg.Envelope
	if env == nil {
		raw, err := marshalItem(item)
		if err != nil {
			return source.Input{}, err
		}
		in.Payload = raw
		return in, nil
	}
	if text, ok := fieldText(item, env.Type); ok {
		in.Type = text
	}
	if text, ok := fieldText(item, env.DeliveryID); ok {
		in.DeliveryID = text
	}
	value := item
	if env.Payload != nil && !env.Payload.Whole {
		found, ok := sourcespec.LookupIn(item, env.Payload.Pointer)
		if !ok {
			return source.Input{}, fmt.Errorf("payload 指针 %q 在该条 item 上取不到", env.Payload.Pointer)
		}
		value = found
	}
	raw, err := marshalItem(value)
	if err != nil {
		return source.Input{}, err
	}
	in.Payload = raw
	return in, nil
}

func fieldText(item any, field *sourcespec.FieldSource) (string, bool) {
	if field == nil {
		return "", false
	}
	text := ""
	if field.Pointer != "" {
		if value, ok := sourcespec.LookupIn(item, field.Pointer); ok {
			text, _ = sourcespec.ScalarText(value)
		}
	}
	if joined, ok := joinText(item, field.JoinFrom); ok && text != "" && joined != "" {
		text += "." + joined
	}
	return text, text != ""
}

func joinText(item any, pointer string) (string, bool) {
	if pointer == "" {
		return "", false
	}
	value, ok := sourcespec.LookupIn(item, pointer)
	if !ok {
		return "", false
	}
	text, ok := sourcespec.ScalarText(value)
	return text, ok
}

func marshalItem(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("item 无法编码成 JSON: %w", err)
	}
	return raw, nil
}
