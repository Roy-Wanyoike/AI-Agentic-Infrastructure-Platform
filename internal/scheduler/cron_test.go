package scheduler

import (
        "testing"
        "time"
)

// TestParseCronValid covers the pinned grammar: '*', numbers, ranges,
// lists and steps (including combinations).
func TestParseCronValid(t *testing.T) {
        valid := []string{
                "* * * * *",
                "0 * * * *",
                "59 23 31 12 6",
                "0-29 * * * *",       // range
                "0,15,30,45 * * * *", // list
                "*/15 * * * *",       // step over star
                "10-50/5 * * * *",    // step over range
                "5/10 * * * *",       // single value + step => value..max/step
                "0 9-17 * * 1-5",     // business hours
                "30 2 1,15 * 0",      // dom restricted + dow restricted (OR rule)
                "0 0 1 1 *",
                "1-5,10-15,20 * * * *", // list of ranges
                "0 */6 * * *",          // every six hours
        }
        for _, expr := range valid {
                if err := ParseCron(expr); err != nil {
                        t.Errorf("ParseCron(%q) returned error: %v", expr, err)
                }
        }
}

// TestParseCronInvalid rejects malformed expressions: wrong field count,
// out-of-range values, inverted ranges, bad steps and empty items.
func TestParseCronInvalid(t *testing.T) {
        invalid := []string{
                "",                       // empty
                "* * * *",                // 4 fields
                "* * * * * *",            // 6 fields
                "60 * * * *",             // minute 60
                "-1 * * * *",             // negative
                "* 24 * * *",             // hour 24
                "* * 0 * *",              // dom 0
                "* * 32 * *",             // dom 32
                "* * * 0 *",              // month 0
                "* * * 13 *",             // month 13
                "* * * * 7",              // dow 7 (0-6 only in the minimal grammar)
                "10-5 * * * *",           // inverted range
                "*/0 * * * *",            // zero step
                "*/-3 * * * *",           // negative step
                "1,,3 * * * *",           // empty list item
                "1, * * * *",             // empty list item (space variant)
                "abc * * * *",            // non-numeric
                "1- * * * *",             // dangling range
                "1/ * * * *",             // dangling step
                "1/0-5 * * * *",          // step before slash range invalid
                "   *   *   *   *   *  ", // still valid after Fields()... handled below
        }
        for _, expr := range invalid {
                // The whitespace-only-separated variant is actually valid; skip it.
                if expr == "   *   *   *   *   *  " {
                        if err := ParseCron(expr); err != nil {
                                t.Errorf("ParseCron(%q) should normalize whitespace, got %v", expr, err)
                        }
                        continue
                }
                if err := ParseCron(expr); err == nil {
                        t.Errorf("ParseCron(%q) should have failed", expr)
                }
        }
}

// TestNextCronTimeTable drives NextCronTime over concrete instants, including
// ranges, lists, steps, the dom/dow OR rule and DST-ish edges.
func TestNextCronTimeTable(t *testing.T) {
        utc := time.UTC
        base := time.Date(2025, time.January, 15, 10, 30, 0, 0, time.UTC)

        cases := []struct {
                name   string
                expr   string
                loc    *time.Location
                after  time.Time
                want   time.Time
                wantOK bool
        }{
                {
                        name:   "every minute",
                        expr:   "* * * * *",
                        after:  base,
                        want:   base.Add(time.Minute),
                        wantOK: true,
                },
                {
                        name:   "exact minute next hour",
                        expr:   "15 * * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 15, 11, 15, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "same minute not repeated",
                        expr:   "30 10 * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 16, 10, 30, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "range minutes",
                        expr:   "0-29 * * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 15, 11, 0, 0, 0, utc), // minute 0 is in range at the next hour
                        wantOK: true,
                },
                {
                        name:   "list minutes",
                        expr:   "0,15,30,45 * * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 15, 10, 45, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "step minutes",
                        expr:   "*/15 * * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 15, 10, 45, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "range with step",
                        expr:   "10-50/5 * * * *",
                        after:  base,
                        want:   time.Date(2025, 1, 15, 10, 35, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "specific date",
                        expr:   "0 0 20 1 *",
                        after:  base,
                        want:   time.Date(2025, 1, 20, 0, 0, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "month skip",
                        expr:   "0 0 1 3 *",
                        after:  base,
                        want:   time.Date(2025, 3, 1, 0, 0, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "day-of-week only",
                        expr:   "0 12 * * 1", // Mondays noon
                        after:  base,         // 2025-01-15 is a Wednesday
                        want:   time.Date(2025, 1, 20, 12, 0, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "dom and dow restricted: OR rule",
                        expr:   "0 12 20 * 1", // 20th OR Mondays
                        after:  base,          // next Monday after Jan 15 is Jan 20 (also the 20th!)
                        want:   time.Date(2025, 1, 20, 12, 0, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "dom and dow restricted: dom branch wins first",
                        expr:   "0 12 16 * 1", // 16th OR Mondays; Jan 16 (Thu) precedes Jan 20 (Mon)
                        after:  base,
                        want:   time.Date(2025, 1, 16, 12, 0, 0, 0, utc),
                        wantOK: true,
                },
                {
                        name:   "never fires (Feb 31)",
                        expr:   "0 0 31 2 *",
                        after:  base,
                        wantOK: false,
                },
                {
                        name:   "leap year day",
                        expr:   "0 0 29 2 *",
                        after:  base,
                        want:   time.Date(2028, 2, 29, 0, 0, 0, 0, utc),
                        wantOK: true,
                },
        }

        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        loc := tc.loc
                        if loc == nil {
                                loc = time.UTC
                        }
                        got, ok := NextCronTime(tc.expr, loc, tc.after)
                        if ok != tc.wantOK {
                                t.Fatalf("NextCronTime(%q) ok = %v, want %v (got %s)", tc.expr, ok, tc.wantOK, got)
                        }
                        if !tc.wantOK {
                                return
                        }
                        if !got.Equal(tc.want) {
                                t.Fatalf("NextCronTime(%q) = %s, want %s", tc.expr, got, tc.want)
                        }
                        if got.Location() != time.UTC {
                                t.Fatalf("NextCronTime must return UTC, got %s", got.Location())
                        }
                })
        }
}

// TestNextCronTimeDSTEdges exercises spring-forward and fall-back transitions
// in America/New_York. On the spring-forward day 02:00 local does not exist,
// so the scan rolls to the next day; on the fall-back day the first 01:00
// occurrence fires.
func TestNextCronTimeDSTEdges(t *testing.T) {
        ny, err := time.LoadLocation("America/New_York")
        if err != nil {
                t.Skipf("tzdata unavailable: %v", err)
        }

        // Spring forward: 2025-03-09 02:00 EST does not exist (clocks jump to 03:00 EDT).
        afterGap := time.Date(2025, time.March, 9, 1, 30, 0, 0, ny)
        got, ok := NextCronTime("0 2 * * *", ny, afterGap)
        if !ok {
                t.Fatal("expected a next run after the spring-forward gap")
        }
        if wall := got.In(ny); wall.Day() != 10 || wall.Hour() != 2 || wall.Minute() != 0 {
                t.Fatalf("expected next fire 2025-03-10 02:00 local, got %s (%s)", wall, got)
        }

        // Regular (non-gap) day resolves to 02:00 EDT = 06:00 UTC.
        regular := time.Date(2025, time.March, 10, 1, 0, 0, 0, ny)
        got, ok = NextCronTime("0 2 * * *", ny, regular)
        if !ok || got != time.Date(2025, time.March, 10, 6, 0, 0, 0, time.UTC) {
                t.Fatalf("expected 2025-03-10T06:00:00Z, got %s ok=%v", got, ok)
        }

        // Fall back: 2025-11-02 01:30 happens twice; a 01:30 schedule fires on the
        // FIRST occurrence and — matching classic Vixie cron semantics — again on
        // the repeated 01:30 after clocks fall back, then the following day.
        after := time.Date(2025, time.November, 2, 0, 30, 0, 0, ny)
        got, ok = NextCronTime("30 1 * * *", ny, after)
        if !ok {
                t.Fatal("expected a next run on the fall-back day")
        }
        wall := got.In(ny)
        if wall.Hour() != 1 || wall.Minute() != 30 || wall.Day() != 2 {
                t.Fatalf("expected first 01:30 local occurrence on 2025-11-02, got %s (%s)", wall, got)
        }
        // The repeated wall-clock hour is the very next match (Vixie behavior).
        second, ok2 := NextCronTime("30 1 * * *", ny, got)
        if !ok2 {
                t.Fatal("expected the repeated 01:30 occurrence after fall-back")
        }
        if second.Sub(got) != time.Hour {
                t.Fatalf("repeated occurrence should be exactly one hour later, got %s -> %s", got, second)
        }
        third, ok3 := NextCronTime("30 1 * * *", ny, second)
        if !ok3 {
                t.Fatal("expected a following occurrence")
        }
        if thirdWall := third.In(ny); thirdWall.Day() != 3 {
                t.Fatalf("occurrence after the repeated hour must be the following day, got %s", thirdWall)
        }
}

// TestNextCronTimeTimezoneAwareness: 09:00 in Asia/Kolkata is 03:30 UTC.
func TestNextCronTimeTimezoneAwareness(t *testing.T) {
        kolkata, err := time.LoadLocation("Asia/Kolkata")
        if err != nil {
                t.Skipf("tzdata unavailable: %v", err)
        }
        after := time.Date(2025, time.June, 2, 5, 0, 0, 0, time.UTC) // 10:30 IST
        got, ok := NextCronTime("0 9 * * *", kolkata, after)
        if !ok {
                t.Fatal("expected next run")
        }
        want := time.Date(2025, time.June, 3, 3, 30, 0, 0, time.UTC) // 09:00 IST next day
        if !got.Equal(want) {
                t.Fatalf("expected %s, got %s", want, got)
        }
}
