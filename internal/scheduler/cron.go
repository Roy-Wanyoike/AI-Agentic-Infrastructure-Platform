package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cron.go implements a minimal 5-field cron parser with NO external
// dependencies (wave-2 contract, track 2-f).
//
// Grammar (minute hour day-of-month month day-of-week):
//
//      field := "*" | item ("," item)*
//      item  := (number | number "-" number) [ "/" step ] | "*" [ "/" step ]
//
// Field ranges: minute 0-59, hour 0-23, day-of-month 1-31, month 1-12,
// day-of-week 0-6 (Sunday = 0). Steps must be positive integers. Each field
// is expanded into a boolean lookup table so matching and next-run scans are
// O(1) per candidate minute.
//
// Semantics notes:
//   - day-of-month and day-of-week follow the classic Vixie cron rule: when
//     both fields are restricted (neither is "*") a day matches if EITHER
//     field matches; otherwise both must match.
//   - Next(...) evaluates wall-clock time in the schedule's location, so DST
//     transitions are respected: non-existent wall times (spring-forward gap)
//     normalize forward and the scan continues from the normalized instant.

const (
	cronMinMinute = 0
	cronMaxMinute = 59
	cronMinHour   = 0
	cronMaxHour   = 23
	cronMinDom    = 1
	cronMaxDom    = 31
	cronMinMonth  = 1
	cronMaxMonth  = 12
	cronMinDow    = 0
	cronMaxDow    = 6
)

// cronHorizon bounds the next-run scan: an expression that never fires within
// five years is treated as never-firing and rejected at create time.
const cronHorizonYears = 5

var errCronField = errors.New("invalid cron expression")

type cronExpr struct {
	minute  []bool // index 0 = minute 0
	hour    []bool // index 0 = hour 0
	dom     []bool // index 0 = day 1
	month   []bool // index 0 = January
	dow     []bool // index 0 = Sunday (time.Weekday)
	domStar bool
	dowStar bool
}

// parseCron validates and compiles a 5-field cron expression.
func parseCron(expr string) (*cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%w: expected 5 fields (minute hour day-of-month month day-of-week), got %d", errCronField, len(fields))
	}
	minute, err := parseCronField(fields[0], cronMinMinute, cronMaxMinute)
	if err != nil {
		return nil, fmt.Errorf("minute field %q: %w", fields[0], err)
	}
	hour, err := parseCronField(fields[1], cronMinHour, cronMaxHour)
	if err != nil {
		return nil, fmt.Errorf("hour field %q: %w", fields[1], err)
	}
	dom, err := parseCronField(fields[2], cronMinDom, cronMaxDom)
	if err != nil {
		return nil, fmt.Errorf("day-of-month field %q: %w", fields[2], err)
	}
	month, err := parseCronField(fields[3], cronMinMonth, cronMaxMonth)
	if err != nil {
		return nil, fmt.Errorf("month field %q: %w", fields[3], err)
	}
	dow, err := parseCronField(fields[4], cronMinDow, cronMaxDow)
	if err != nil {
		return nil, fmt.Errorf("day-of-week field %q: %w", fields[4], err)
	}
	return &cronExpr{
		minute:  minute,
		hour:    hour,
		dom:     dom,
		month:   month,
		dow:     dow,
		domStar: allTrue(dom[:]),
		dowStar: allTrue(dow[:]),
	}, nil
}

// parseCronField expands one cron field into a boolean table indexed by
// (value - min).
func parseCronField(field string, min, max int) ([]bool, error) {
	table := make([]bool, max-min+1)
	if strings.TrimSpace(field) == "" {
		return table, fmt.Errorf("%w: empty field", errCronField)
	}
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return table, fmt.Errorf("%w: empty list item", errCronField)
		}
		base, stepStr := item, ""
		if i := strings.Index(item, "/"); i >= 0 {
			base, stepStr = item[:i], item[i+1:]
			if stepStr == "" {
				return table, fmt.Errorf("%w: step requires a value after '/'", errCronField)
			}
		}
		step := 1
		if stepStr != "" {
			v, err := strconv.Atoi(stepStr)
			if err != nil || v <= 0 {
				return table, fmt.Errorf("%w: invalid step %q", errCronField, stepStr)
			}
			step = v
		}
		lo, hi := min, max
		switch {
		case base == "*":
			// full range
		case strings.Contains(base, "-"):
			parts := strings.SplitN(base, "-", 2)
			start, err := strconv.Atoi(parts[0])
			if err != nil {
				return table, fmt.Errorf("%w: invalid range start %q", errCronField, parts[0])
			}
			end, err := strconv.Atoi(parts[1])
			if err != nil {
				return table, fmt.Errorf("%w: invalid range end %q", errCronField, parts[1])
			}
			if start < min || end > max || start > end {
				return table, fmt.Errorf("%w: range %d-%d outside %d-%d or inverted", errCronField, start, end, min, max)
			}
			lo, hi = start, end
		default:
			v, err := strconv.Atoi(base)
			if err != nil {
				return table, fmt.Errorf("%w: invalid number %q", errCronField, base)
			}
			if v < min || v > max {
				return table, fmt.Errorf("%w: value %d outside %d-%d", errCronField, v, min, max)
			}
			lo, hi = v, v
			if stepStr != "" {
				// Vixie semantics: "N/step" expands to "N-max/step".
				hi = max
			}
		}
		for v := lo; v <= hi; v += step {
			table[v-min] = true
		}
	}
	return table, nil
}

func allTrue(values []bool) bool {
	for _, v := range values {
		if !v {
			return false
		}
	}
	return len(values) > 0
}

// matches reports whether the compiled expression matches the wall-clock time
// t (t is evaluated in whatever location it carries).
func (c *cronExpr) matches(t time.Time) bool {
	if !c.month[int(t.Month())-1] {
		return false
	}
	if !c.dayMatches(t) {
		return false
	}
	if !c.hour[t.Hour()] {
		return false
	}
	return c.minute[t.Minute()]
}

// dayMatches implements the Vixie cron day rule: if both day-of-month and
// day-of-week are restricted, either match fires; if one is '*', only the
// restricted field must match.
func (c *cronExpr) dayMatches(t time.Time) bool {
	domOK := c.dom[t.Day()-1]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the next instant strictly after `after` that matches the
// expression, evaluated in loc and returned as UTC. ok is false when the
// expression never fires within cronHorizonYears (e.g. "0 0 31 2 *").
func (c *cronExpr) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	// Start from the minute strictly after `after`. Truncate works on the
	// absolute timeline, which is what we want: reconstructing the wall clock
	// via time.Date would re-select the earlier offset for ambiguous local
	// times (fall-back) and could return the same occurrence twice.
	cur := after.In(loc).Truncate(time.Minute)
	if !cur.After(after) {
		cur = cur.Add(time.Minute)
	}
	limit := after.In(loc).AddDate(cronHorizonYears, 0, 0)
	for cur.Before(limit) {
		if !c.month[int(cur.Month())-1] {
			// Skip to the first minute of the next month.
			cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatches(cur) {
			// Skip to midnight of the next day.
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !c.hour[cur.Hour()] {
			// Skip to the top of the next hour.
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if c.minute[cur.Minute()] {
			return cur.UTC(), true
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}, false
}

// ParseCron validates a 5-field cron expression and returns nil when valid.
func ParseCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

// NextCronTime computes the next matching instant (UTC) strictly after
// `after` for a 5-field cron expression evaluated in loc. ok is false when
// the expression never fires within the 5-year horizon.
func NextCronTime(expr string, loc *time.Location, after time.Time) (time.Time, bool) {
	parsed, err := parseCron(expr)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.Next(after, loc)
}
