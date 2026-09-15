package logs

import (
	"reflect"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/store"
)

func TestParseStatusFilter(t *testing.T) {
	type c = store.StatusCond
	ok := map[string][]c{
		"":         nil,
		"502":      {{Op: "=", Value: 502}},
		">=500":    {{Op: ">=", Value: 500}},
		"≥500":     {{Op: ">=", Value: 500}},
		"< 400":    {{Op: "<", Value: 400}},
		"4xx":      {{Op: "class", Value: 4}},
		"5XX":      {{Op: "class", Value: 5}},
		"!200":     {{Op: "!=", Value: 200}},
		"!=304":    {{Op: "!=", Value: 304}},
		"!5xx":     {{Op: "!class", Value: 5}},
		">4xx":     {{Op: ">=", Value: 500}},
		">=4xx":    {{Op: ">=", Value: 400}},
		"<=3xx":    {{Op: "<", Value: 400}},
		"401,403":  {{Op: "=", Value: 401}, {Op: "=", Value: 403}},
		"5xx,!502": {{Op: "class", Value: 5}, {Op: "!=", Value: 502}},
	}
	for in, want := range ok {
		got, err := ParseStatusFilter(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
	for _, in := range []string{"abc", "6xx", ">=", "!>=500", "5x", "1000", "-1"} {
		if _, err := ParseStatusFilter(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestStatusSQL(t *testing.T) {
	conds, _ := ParseStatusFilter("5xx,!502,!504")
	sql, args := store.StatusSQL(conds)
	if sql != "((status >= ? AND status < ?)) AND status != ? AND status != ?" {
		t.Fatalf("sql: %s", sql)
	}
	if !reflect.DeepEqual(args, []any{500, 600, 502, 504}) {
		t.Fatalf("args: %v", args)
	}
	conds, _ = ParseStatusFilter("401,403")
	if sql, _ := store.StatusSQL(conds); sql != "(status = ? OR status = ?)" {
		t.Fatalf("sql: %s", sql)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     {},
		"15m":                  now.Add(-15 * time.Minute),
		"1h":                   now.Add(-time.Hour),
		"24h":                  now.Add(-24 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2w":                   now.Add(-14 * 24 * time.Hour),
		"2026-09-14T10:00:00Z": time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		"2026-09-13":           time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		"1789394400000":        time.UnixMilli(1789394400000),
	}
	for in, want := range cases {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: got %v, %v want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"yesterday", "-1h", "7x"} {
		if _, err := ParseSince(in, now); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}
