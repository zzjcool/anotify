package sourcespec

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // 测试自带 IANA 时区数据，宿主镜像缺 zoneinfo 也照样跑 DST 用例
)

// TestCronDomDowStep 对照 cronie 的 dom/dow 判据：`*/N` 的步长必须进位图，不能当纯 `*`（#610）。
func TestCronDomDowStep(t *testing.T) {
	cases := []struct {
		expr  string
		after string
		want  []string
	}{
		{
			expr:  "0 0 */2 * *",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-03T00:00:00Z", "2026-11-05T00:00:00Z", "2026-11-07T00:00:00Z", "2026-11-09T00:00:00Z", "2026-11-11T00:00:00Z"},
		},
		{
			expr:  "0 0 */10 * *",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-11T00:00:00Z", "2026-11-21T00:00:00Z", "2026-12-01T00:00:00Z", "2026-12-11T00:00:00Z", "2026-12-21T00:00:00Z"},
		},
		{
			expr:  "0 0 */10 * *",
			after: "2026-11-25T00:16:00Z",
			want:  []string{"2026-12-01T00:00:00Z"},
		},
		{
			expr:  "0 9 * * */3",
			after: "2026-11-24T00:00:00Z",
			want:  []string{"2026-11-25T09:00:00Z", "2026-11-28T09:00:00Z", "2026-11-29T09:00:00Z", "2026-12-02T09:00:00Z", "2026-12-05T09:00:00Z"},
		},
		{
			expr:  "0 0 */2 * 1",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-09T00:00:00Z", "2026-11-23T00:00:00Z", "2026-12-07T00:00:00Z", "2026-12-21T00:00:00Z", "2027-01-11T00:00:00Z"},
		},
		{
			expr:  "0 0 */2 * */3",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-07T00:00:00Z", "2026-11-11T00:00:00Z", "2026-11-15T00:00:00Z", "2026-11-21T00:00:00Z", "2026-11-25T00:00:00Z"},
		},
		{
			expr:  "0 0 1 * 1",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-02T00:00:00Z", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z", "2026-11-30T00:00:00Z"},
		},
		{
			expr:  "0 0 * * *",
			after: "2026-11-25T00:16:00Z",
			want:  []string{"2026-11-26T00:00:00Z", "2026-11-27T00:00:00Z", "2026-11-28T00:00:00Z", "2026-11-29T00:00:00Z", "2026-11-30T00:00:00Z"},
		},
		{
			expr:  "0 0 1-31/2 * *",
			after: "2026-11-01T00:00:00Z",
			want:  []string{"2026-11-03T00:00:00Z", "2026-11-05T00:00:00Z", "2026-11-07T00:00:00Z", "2026-11-09T00:00:00Z", "2026-11-11T00:00:00Z"},
		},
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", c.expr, err)
		}
		at := timeAt(c.after)
		got := make([]string, 0, len(c.want))
		for range c.want {
			at = s.Next(at)
			if at.IsZero() {
				t.Fatalf("%q 在 %s 之后只触发 %d 次就永不成立（want %d 次），已得 %v", c.expr, c.after, len(got), len(c.want), got)
			}
			got = append(got, at.Format(time.RFC3339))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q 在 %s 之后的连续触发 = %v, want %v", c.expr, c.after, got, c.want)
		}
	}
}

// 春季跳格的墙钟不存在 → 顺延到跳格结束后第一个存在的时刻（当天仍触发一次，不是整天跳过）；秋季回拨里重复的墙钟只按第一次出现触发一次（#603）。
func TestCronNextDST(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		from string
		want []string
	}{
		{
			name: "new-york spring forward",
			cfg:  `{"expr":"30 2 * * *","tz":"America/New_York"}`,
			from: "2026-03-08T00:00:00-05:00",
			want: []string{"2026-03-08T03:00:00-04:00", "2026-03-09T02:30:00-04:00"},
		},
		{
			name: "new-york fall back",
			cfg:  `{"expr":"30 1 * * *","tz":"America/New_York"}`,
			from: "2026-11-01T00:00:00-04:00",
			want: []string{"2026-11-01T01:30:00-04:00", "2026-11-02T01:30:00-05:00"},
		},
		{
			name: "new-york spring forward control",
			cfg:  `{"expr":"0 3 * * *","tz":"America/New_York"}`,
			from: "2026-03-08T00:00:00-05:00",
			want: []string{"2026-03-08T03:00:00-04:00", "2026-03-09T03:00:00-04:00"},
		},
		{
			name: "london spring forward",
			cfg:  `{"expr":"30 1 * * *","tz":"Europe/London"}`,
			from: "2026-03-29T00:00:00Z",
			want: []string{"2026-03-29T02:00:00+01:00", "2026-03-30T01:30:00+01:00"},
		},
		{
			name: "london fall back",
			cfg:  `{"expr":"30 1 * * *","tz":"Europe/London"}`,
			from: "2026-10-24T23:00:00Z",
			want: []string{"2026-10-25T01:30:00+01:00", "2026-10-26T01:30:00Z"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, c.from)
			if err != nil {
				t.Fatalf("解析起点: %v", err)
			}
			cfg := json.RawMessage(c.cfg)
			got := make([]string, 0, len(c.want))
			for range c.want {
				next, err := NextCronTime(cfg, at)
				if err != nil {
					t.Fatalf("NextCronTime: %v", err)
				}
				if next.IsZero() {
					t.Fatalf("%s: 只触发 %d 次就永不成立（want %d）", c.cfg, len(got), len(c.want))
				}
				got = append(got, next.Format(time.RFC3339))
				at = next
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s 从 %s 起的连续触发 = %v, want %v", c.cfg, c.from, got, c.want)
			}
		})
	}
}
