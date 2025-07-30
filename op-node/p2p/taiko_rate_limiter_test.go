package p2p

import (
	"testing"
	"time"
)

const eps = 1e-9

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func TestRefillInitializesLastAndBoundsCredit(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	b := &rateBucket{credit: 9999} // overfull
	refillBucket(b, now, 10.0, 5.0)
	if !b.last.Equal(now) {
		t.Fatalf("expected last to be set to now")
	}
	if !approxEqual(b.credit, 5.0) {
		t.Fatalf("expected credit bounded to 5, got %f", b.credit)
	}
}

func TestRefillAddsOverTimeAndCaps(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	b := &rateBucket{credit: 0, last: start}

	// 2 seconds at 3 tokens/sec -> +6 tokens, but cap at 5
	refillBucket(b, start.Add(2*time.Second), 3.0, 5.0)
	if !approxEqual(b.credit, 5.0) {
		t.Fatalf("expected credit=5 (capped), got %f", b.credit)
	}
}

func TestRefillNoChangeWhenElapsedNonPositive(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	b := &rateBucket{credit: 2.5, last: now}

	// same timestamp
	refillBucket(b, now, 10.0, 10.0)
	if !approxEqual(b.credit, 2.5) {
		t.Fatalf("expected no change, got %f", b.credit)
	}

	// time earlier than last
	refillBucket(b, now.Add(-time.Second), 10.0, 10.0)
	if !approxEqual(b.credit, 2.5) {
		t.Fatalf("expected no change with negative elapsed, got %f", b.credit)
	}
}

func TestConsumeFailsWhenInsufficient(t *testing.T) {
	b := &rateBucket{credit: 0}
	if consumeToken(b, 1.0) {
		t.Fatalf("consume should fail with 0 credit")
	}
	if !approxEqual(b.credit, 0.0) {
		t.Fatalf("credit should remain unchanged on failed consume, got %f", b.credit)
	}
}

func TestConsumeAfterRefill(t *testing.T) {
	start := time.Unix(3000, 0).UTC()
	b := &rateBucket{credit: 0, last: start}
	// Refill 0.5 sec at 2 tokens/sec -> +1.0
	refillBucket(b, start.Add(500*time.Millisecond), 2.0, 5.0)
	if !approxEqual(b.credit, 1.0) {
		t.Fatalf("expected credit=1.0, got %f", b.credit)
	}
	if !consumeToken(b, 1.0) {
		t.Fatalf("consume should succeed with 1.0 credit")
	}
	if !approxEqual(b.credit, 0.0) {
		t.Fatalf("expected credit=0 after consume, got %f", b.credit)
	}
}

func TestMultipleSmallRefillsAccumulate(t *testing.T) {
	start := time.Unix(4000, 0).UTC()
	b := &rateBucket{credit: 0, last: start}

	rate := 1.0 // 1 token/sec
	max := 5.0

	// 100ms x 5 -> 0.1*5 = 0.5 tokens
	for i := 1; i <= 5; i++ {
		refillBucket(b, start.Add(time.Duration(i)*100*time.Millisecond), rate, max)
	}
	if !approxEqual(b.credit, 0.5) {
		t.Fatalf("expected 0.5 tokens after 5x100ms, got %f", b.credit)
	}
}

func TestCapIsRespectedAfterManySeconds(t *testing.T) {
	start := time.Unix(5000, 0).UTC()
	b := &rateBucket{credit: 0, last: start}
	// 100 seconds at 0.2 tokens/sec -> 20, but cap at 5
	refillBucket(b, start.Add(100*time.Second), 0.2, 5.0)
	if !approxEqual(b.credit, 5.0) {
		t.Fatalf("expected credit capped at 5, got %f", b.credit)
	}
}
