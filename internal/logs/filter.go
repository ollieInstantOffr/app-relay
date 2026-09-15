package logs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/store"
)

// ParseStatusFilter parses status expressions used by the access log filter:
//
//	"502"        exact
//	">=500"      comparison (>, >=, <, <=, =, !=; ≥ and ≤ accepted)
//	"4xx"        status class
//	"!200" "!5xx" negation
//	"401,403"    comma list: positive terms OR'ed, negated terms AND'ed
func ParseStatusFilter(expr string) ([]store.StatusCond, error) {
	expr = strings.NewReplacer("≥", ">=", "≤", "<=", " ", "").Replace(strings.TrimSpace(expr))
	if expr == "" {
		return nil, nil
	}
	var out []store.StatusCond
	for _, part := range strings.Split(expr, ",") {
		if part == "" {
			continue
		}
		c, err := parseStatusTerm(part)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func parseStatusTerm(term string) (store.StatusCond, error) {
	bad := fmt.Errorf("invalid status filter %q (try 502, >=500, 4xx or !200)", term)
	op := "="
	rest := term
	for _, p := range []string{">=", "<=", "!=", ">", "<", "=", "!"} {
		if strings.HasPrefix(term, p) {
			op, rest = p, term[len(p):]
			break
		}
	}
	if op == "!" {
		op = "!="
	}
	if len(rest) == 3 && strings.EqualFold(rest[1:], "xx") {
		d := rest[0]
		if d < '1' || d > '5' {
			return store.StatusCond{}, bad
		}
		switch op {
		case "=":
			return store.StatusCond{Op: "class", Value: int(d - '0')}, nil
		case "!=":
			return store.StatusCond{Op: "!class", Value: int(d - '0')}, nil
		case ">=", "<", ">", "<=":
			base := int(d-'0') * 100
			switch op {
			case ">":
				return store.StatusCond{Op: ">=", Value: base + 100}, nil
			case "<=":
				return store.StatusCond{Op: "<", Value: base + 100}, nil
			}
			return store.StatusCond{Op: op, Value: base}, nil
		}
		return store.StatusCond{}, bad
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || n > 999 {
		return store.StatusCond{}, bad
	}
	return store.StatusCond{Op: op, Value: n}, nil
}

// ParseSince parses a lower time bound: a duration back from now ("15m",
// "1h", "24h", "7d", "2w"), RFC 3339, a date (2006-01-02) or unix
// milliseconds. Empty returns the zero time.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, ok := parseDuration(s); ok {
		return now.Add(-d), nil
	}
	return parseTimeValue(s)
}

// ParseUntil parses an upper bound: RFC 3339, date or unix ms (durations are
// interpreted as "that long ago").
func ParseUntil(s string, now time.Time) (time.Time, error) { return ParseSince(s, now) }

func parseDuration(s string) (time.Duration, bool) {
	if n := len(s); n >= 2 {
		unit := s[n-1]
		if unit == 'd' || unit == 'w' {
			v, err := strconv.ParseFloat(s[:n-1], 64)
			if err != nil || v < 0 {
				return 0, false
			}
			day := 24 * time.Hour
			if unit == 'w' {
				day *= 7
			}
			return time.Duration(v * float64(day)), true
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

func parseTimeValue(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use 1h, 24h, 7d or RFC 3339)", s)
}
