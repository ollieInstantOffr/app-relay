package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIsThrottled(t *testing.T) {
	cases := []struct {
		c    throttleCounts
		max  int
		want bool
	}{
		{throttleCounts{0, 0}, 3, false},
		{throttleCounts{2, 2}, 3, false},
		{throttleCounts{3, 3}, 3, true},
		{throttleCounts{1, 14}, 3, false},
		{throttleCounts{1, 15}, 3, true},
		{throttleCounts{3, 3}, 0, true}, // unset max falls back to 3
		{throttleCounts{5, 5}, 10, false},
	}
	for _, c := range cases {
		if got := isThrottled(c.c, c.max); got != c.want {
			t.Errorf("isThrottled(%+v, %d) = %v, want %v", c.c, c.max, got, c.want)
		}
	}
}

func TestLoginThrottleWindow(t *testing.T) {
	s, clock := newTestService(t)
	ctx := context.Background()
	st := s.app.Store
	const ip = "203.0.113.88"

	fail := func(user string) {
		t.Helper()
		if err := st.RecordLoginAttempt(ctx, clock.now(), ip, user, false); err != nil {
			t.Fatal(err)
		}
	}

	// Defaults: 3 attempts, 15 minute lockout.
	fail("mira")
	fail("mira")
	if s.throttled(ctx, ip, "mira") {
		t.Fatal("throttled after 2 failures")
	}
	fail("mira")
	if !s.throttled(ctx, ip, "mira") {
		t.Fatal("not throttled after 3 failures")
	}
	if s.throttled(ctx, "192.168.1.24", "mira") {
		t.Fatal("another address must not be throttled for the same user")
	}
	if s.throttled(ctx, ip, "jonas") {
		t.Fatal("another user from the same address must not be throttled by the pair rule")
	}

	clock.advance(10 * time.Minute)
	if !s.throttled(ctx, ip, "mira") {
		t.Fatal("throttle lifted before the lockout window elapsed")
	}
	clock.advance(6 * time.Minute)
	if s.throttled(ctx, ip, "mira") {
		t.Fatal("still throttled after the lockout window")
	}

	// A success resets the pair count.
	fail("jonas")
	fail("jonas")
	if err := st.RecordLoginAttempt(ctx, clock.now().Add(time.Millisecond), ip, "jonas", true); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	fail("jonas")
	fail("jonas")
	if s.throttled(ctx, ip, "jonas") {
		t.Fatal("failures before a success must not count")
	}

	// Spraying many usernames from one address trips the per-IP limit.
	clock.advance(time.Hour)
	for i := 0; i < 3*ipFailureMultiplier; i++ {
		fail(fmt.Sprintf("user%d", i))
	}
	if !s.throttled(ctx, ip, "brand-new-user") {
		t.Fatal("per-address limit not enforced")
	}
}
