package sourcespec

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cron 表达式（五段）：支持 `* / 5 / 1-5 / */15 / 1,3,5 / 1-30/5`；`@daily` 宏、秒字段、L/W/# 明确拒绝（静默忽略比报错糟得多）。只做时刻计算，不理解业务语义。

type CronSchedule struct {
	Minute, Hour, Dom, Month, Dow map[int]bool
	// DomStar / DowStar：字段是否以 `*` 开头，只决定 dom/dow 的组合方式（任一以 `*` 开头 → AND，都受限 → OR），不表示「不用判断」：`*/2` 只匹配奇数日。
	DomStar, DowStar bool
}

func ParseCron(expr string) (*CronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式需要 5 段（分 时 日 月 周），得到 %d 段: %q", len(fields), expr)
	}
	var (
		s   = &CronSchedule{}
		err error
	)
	if s.Minute, _, err = parseCronField(fields[0], 0, 59, "分"); err != nil {
		return nil, err
	}
	if s.Hour, _, err = parseCronField(fields[1], 0, 23, "时"); err != nil {
		return nil, err
	}
	if s.Dom, s.DomStar, err = parseCronField(fields[2], 1, 31, "日"); err != nil {
		return nil, err
	}
	if s.Month, _, err = parseCronField(fields[3], 1, 12, "月"); err != nil {
		return nil, err
	}
	if s.Dow, s.DowStar, err = parseCronField(fields[4], 0, 7, "周"); err != nil {
		return nil, err
	}
	if s.Dow[7] { // 7 也表示周日（两套惯例都有人用）
		s.Dow[0] = true
		delete(s.Dow, 7)
	}
	return s, nil
}

func parseCronField(field string, min, max int, name string) (map[int]bool, bool, error) {
	out := map[int]bool{}
	if field == "" {
		return nil, false, fmt.Errorf("cron %s 字段为空", name)
	}
	isStar := strings.HasPrefix(field, "*")

	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, false, fmt.Errorf("cron %s 字段有空项: %q", name, field)
		}
		// 步长：base/step
		base, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			v, err := strconv.Atoi(stepStr)
			if err != nil || v <= 0 {
				return nil, false, fmt.Errorf("cron %s 字段步长非法: %q", name, part)
			}
			step = v
		}

		lo, hi := min, max
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			a, b, ok := strings.Cut(base, "-")
			if !ok {
				return nil, false, fmt.Errorf("cron %s 字段区间非法: %q", name, part)
			}
			x, err1 := strconv.Atoi(a)
			y, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return nil, false, fmt.Errorf("cron %s 字段区间非法: %q", name, part)
			}
			lo, hi = x, y
		default:
			// 单值；带步长时表示"从该值到上界"（与 crontab 一致：5/15 = 5,20,35,50）
			v, err := strconv.Atoi(base)
			if err != nil {
				return nil, false, fmt.Errorf("cron %s 字段无法解析: %q（%s）", name, part, "只支持数字、*、范围、列表、步长")
			}
			if hasStep {
				lo, hi = v, max
			} else {
				lo, hi = v, v
			}
		}

		if lo < min || hi > max || lo > hi {
			return nil, false, fmt.Errorf("cron %s 字段超出范围 [%d,%d]: %q", name, min, max, part)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, false, fmt.Errorf("cron %s 字段没有匹配值: %q", name, field)
	}
	return out, isStar, nil
}

// Next 返回 after 之后的第一个触发时刻（严格大于 after，秒/纳秒归零）；没有（如 2 月 30 日）时返回零值。
//
// 按**墙钟**匹配（`30 2 * * *` 指本地 02:30，不是固定偏移）：日历推进用 UTC 承载（无夏令时，
// 不会被跳格/回拨改写），分/时都匹配后才翻译成 after 所在时区的绝对时刻——春季跳格里不存在的
// 墙钟顺延到跳格结束后第一个存在的时刻（当天仍触发一次），秋季回拨里重复的墙钟只在第一次出现时
// 触发一次。上限 5 年，跨得过任何闰年组合又不死循环。
func (s *CronSchedule) Next(after time.Time) time.Time {
	loc := after.Location()
	c := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, time.UTC).
		Add(time.Minute)

	limit := c.AddDate(5, 0, 0)
	for c.Before(limit) {
		if !s.Month[int(c.Month())] {
			c = time.Date(c.Year(), c.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.matchDay(c) {
			c = time.Date(c.Year(), c.Month(), c.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.Hour[c.Hour()] {
			c = time.Date(c.Year(), c.Month(), c.Day(), c.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if !s.Minute[c.Minute()] {
			c = c.Add(time.Minute)
			continue
		}
		fire, ok := wallClockInstant(loc, c)
		if !ok {
			c = time.Date(c.Year(), c.Month(), c.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if fire.After(after) {
			return fire
		}
		// 已经落在 after 之前（回拨里「第一次出现」被越过）：从它的墙钟继续往后找。
		c = time.Date(fire.Year(), fire.Month(), fire.Day(), fire.Hour(), fire.Minute(), 0, 0, time.UTC).Add(time.Minute)
	}
	return time.Time{}
}

// wallClockInstant 把墙钟游标 c 翻译成 loc 里的绝对时刻：出现两次（回拨）取第一次，往前半小时/1/2
// 小时的绝对时刻墙钟相同即当前是第二次；不存在（跳格）取同一天里墙钟不早于它、且真实存在的第一个时刻。
// false = 这个墙钟在 loc 里整个不存在（整日被跳过的时区），调用方跳过这一格。
func wallClockInstant(loc *time.Location, c time.Time) (time.Time, bool) {
	y, mo, d := c.Date()
	h, mi := c.Hour(), c.Minute()
	t := time.Date(y, mo, d, h, mi, 0, 0, loc)
	if t.Hour() == h && t.Minute() == mi {
		for _, back := range [...]time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour} {
			if prev := t.Add(-back); prev.Hour() == h && prev.Minute() == mi {
				return prev, true
			}
		}
		return t, true
	}
	target := h*60 + mi
	start := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	for i := 0; i < 24*60; i++ {
		cand := start.Add(time.Duration(i) * time.Minute)
		cy, cmo, cd := cand.Date()
		if cy != y || cmo != mo || cd != d {
			continue
		}
		if cand.Hour()*60+cand.Minute() >= target {
			return cand, true
		}
	}
	return time.Time{}, false
}

// matchDay 判 dom/dow（cronie）：任一字段以 `*` 开头 → 两者 AND，都受限 → OR；位图始终参与。
func (s *CronSchedule) matchDay(t time.Time) bool {
	domOK, dowOK := s.Dom[t.Day()], s.Dow[int(t.Weekday())]
	if s.DomStar || s.DowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// NextCronTime 按源的 config 算下次触发时刻（供 store 与调度器共用）。tz 为空或 "Local" 时用本地时区。
// 返回零值时调用方应当放弃调度并记日志（表达式永不成立），而不是把它当成「0 秒」。
func NextCronTime(config json.RawMessage, after time.Time) (time.Time, error) {
	c := ParseConfig(KindCron, config)
	expr := strings.TrimSpace(c.Expr)
	if expr == "" {
		return time.Time{}, errf("cron 需要 expr（五段表达式，如 0 3 * * *）")
	}
	loc := after.Location()
	if c.TZ != "" && c.TZ != "Local" {
		l, err := time.LoadLocation(c.TZ)
		if err != nil {
			return time.Time{}, errf("未知时区 %q", c.TZ)
		}
		loc = l
	}
	sched, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.In(loc)), nil
}
