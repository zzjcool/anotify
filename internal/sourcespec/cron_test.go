package sourcespec

import (
	"slices"
	"testing"
	"time"
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
