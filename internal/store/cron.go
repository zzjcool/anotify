package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cron 表达式解析（五段：分 时 日 月 周）。
//
// 为什么自己写而不引第三方：只需要"算出下次触发时刻"这一件事，
// 而 crontab 语法里真正会被用到的就是 `* / , -` 这几样。引一个库要为
// 它承担版本与安全面，收益不成正比。
//
// **不变量 I6 的边界**：这里只做"时刻计算"，不理解任何业务语义、
// 不执行任何东西。表达式里除了数字和 * / , - 之外的字符一律拒绝。
//
// 支持的写法：
//
//	*            任意
//	5            确切值
//	1-5          区间
//	*/15         步长
//	1,3,5        列表
//	1-30/5       区间 + 步长
//
// 不支持（明确拒绝，不静默忽略）：
//
//	@daily 之类的宏、秒字段（6 段）、L/W/# 这些扩展语法
//
// 这些不支持的东西如果被静默忽略，用户会以为配好了但实际没按预期触发——
// 那比报错糟得多。

// CronSchedule 是一个解析好的五段表达式。
type CronSchedule struct {
	Minute, Hour, Dom, Month, Dow map[int]bool
	// DomStar / DowStar 记录这两个字段是不是 `*`。
	//
	// 标准 cron 的语义：dom 和 dow 都受限时是 **OR**（"每月 1 号" 或 "每周一"），
	// 只有一个受限时是 AND（即另一个必须是 *）。要正确表达 OR 语义，
	// 必须知道哪个是通配。
	DomStar, DowStar bool
}

// ParseCron 解析五段表达式。
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
	// 7 也表示周日（两套惯例都有人用）
	if s.Dow[7] {
		s.Dow[0] = true
		delete(s.Dow, 7)
	}
	return s, nil
}

// parseCronField 解析单个字段，返回允许的取值集合与"是否为 *"。
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
			// 用默认区间
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

// Next 返回 after 之后的第一个触发时刻（严格大于 after，秒/纳秒归零）。
//
// 找不到（例如表达式永不成立，如 2 月 30 日）时返回零值 time.Time。
func (s *CronSchedule) Next(after time.Time) time.Time {
	// 从下一分钟开始：cron 的语义是"到点触发"，同一分钟内不重复
	t := after.Truncate(time.Minute).Add(time.Minute)

	// 上限 5 年：足够跨过任何闰年组合，也不至于死循环
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !s.Month[int(t.Month())] {
			// 跳到下月 1 日 00:00
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.matchDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if !s.Hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
			continue
		}
		if !s.Minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// matchDay 实现 dom/dow 的 OR 语义（见 CronSchedule 的字段注释）。
func (s *CronSchedule) matchDay(t time.Time) bool {
	domOK := s.Dom[t.Day()]
	dowOK := s.Dow[int(t.Weekday())]
	switch {
	case s.DomStar && s.DowStar:
		return true // 两个都通配：天天都行
	case s.DomStar:
		return dowOK
	case s.DowStar:
		return domOK
	default:
		return domOK || dowOK // 都受限 → OR
	}
}

// NextCronTime 按源的 config 算下次触发时刻（供 store 内部与调度器共用）。
//
// tz 为空或 "Local" 时用本地时区。返回零值时调用方应当放弃调度并记日志
// （表达式永不成立），而不是把它当成"0 秒"。
func NextCronTime(config json.RawMessage, after time.Time) (time.Time, error) {
	c, err := ParseCronConfig(config)
	if err != nil {
		return time.Time{}, err
	}
	loc := after.Location()
	if c.TZ != "" && c.TZ != "Local" {
		l, err := time.LoadLocation(c.TZ)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: 未知时区 %q", ErrInvalidSourceConfig, c.TZ)
		}
		loc = l
	}
	sched, err := ParseCron(c.Expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.In(loc)), nil
}
