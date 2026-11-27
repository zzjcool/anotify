package sourcespec

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/schema"
)

func normalizeWithPayloadSchema /* 真实 config 归一化：创建/更新走的同一条路 */ (kind, raw string) error {
	_, err := NormalizeConfig(kind, json.RawMessage(fmt.Sprintf(`{"payloadSchema":%s}`, raw)))
	return err
}

func TestNormalizeConfigRejectsUnusablePayloadSchema(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string
		says  string
	}{
		{
			name:  "顶层 type 不是 object",
			raw:   `{"type":"string"}`,
			field: "config.payloadSchema.type",
			says:  "schema.type 必须是 object",
		},
		{
			name:  "坏 pattern",
			raw:   `{"type":"object","properties":{"repo":{"type":"string","pattern":"(["}}}`,
			field: "config.payloadSchema.properties.repo.pattern",
			says:  "pattern",
		},
		{
			name:  "pattern 不是字符串",
			raw:   `{"type":"object","properties":{"repo":{"type":"string","pattern":7}}}`,
			field: "config.payloadSchema.properties.repo.pattern",
			says:  "pattern 必须是字符串",
		},
		{
			name:  "required 不是字符串数组",
			raw:   `{"type":"object","required":7}`,
			field: "config.payloadSchema.required",
			says:  "required 必须是字符串数组",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := normalizeWithPayloadSchema(KindManual, tc.raw)
			if err == nil {
				t.Fatal("不可用的 payloadSchema 必须被写入期拒绝")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("错误必须仍属 ErrInvalidConfig（server 据此翻 400），得到 %v", err)
			}
			var causes *CausesError
			if !errors.As(err, &causes) {
				t.Fatalf("错误必须带 causes（红点点到具体那行），得到 %T %v", err, err)
			}
			first := causes.Causes[0]
			if first.Field != tc.field {
				t.Fatalf("causes[0].field = %q，want %q（全部：%v）", first.Field, tc.field, causes.Causes)
			}
			if first.Reason != CauseReasonValueInvalid {
				t.Fatalf("causes[0].reason = %q，want %q", first.Reason, CauseReasonValueInvalid)
			}
			if !strings.Contains(first.Message, tc.says) {
				t.Fatalf("causes[0].message = %q，应说明 %q", first.Message, tc.says)
			}
		})
	}

	err := normalizeWithPayloadSchema /* 数组 / 标量（payloadSchema 本身不是对象）沿用老判据与老措辞 */ (KindManual, `[]`)
	if err == nil || !strings.Contains(err.Error(), "config.payloadSchema 必须是 JSON 对象") {
		t.Fatalf("非对象的 payloadSchema 必须被拒，得到 %v", err)
	}
}

func TestPayloadSchemaVerdictMatchesRuntime(t *testing.T) {
	raws := []string{
		`{}`,
		`{"type":"object"}`,
		`{"type":["object","null"],"properties":{"repo":{"type":"string"}}}`,
		`{"type":"string"}`,
		`{"type":"array"}`,
		`{"type":"object","properties":{"repo":{"type":"weird"}}}`,
		`{"type":"object","properties":{"repo":{"pattern":"(["}}}`,
		`{"type":"object","properties":{"repo":{"pattern":7}}}`,
		`{"type":"object","required":7}`,
	}
	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			_, problems := schema.ParseParams(json.RawMessage(raw))
			runtimeUsable := len(problems) == 0
			writeErr := normalizeWithPayloadSchema(KindManual, raw)
			if (writeErr == nil) != runtimeUsable {
				t.Fatalf("写入期接受=%v，运行期可用=%v（problems=%v，err=%v）",
					writeErr == nil, runtimeUsable, problems, writeErr)
			}
		})
	}
}
