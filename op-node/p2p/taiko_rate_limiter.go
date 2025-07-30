package p2p

import "time"

// rateBucket implements a smooth token bucket:
// - credit: current tokens available (float for smooth refill)
// - last:   last time we updated credit
type rateBucket struct {
	credit float64
	last   time.Time
}

// refillBucket updates b.credit based on elapsed time since b.last.
//   - rate: tokens per second
//   - max:  maximum tokens allowed (bucket capacity)
func refillBucket(b *rateBucket, now time.Time, rate, max float64) {
	if b == nil {
		return
	}
	// Initialize last if zero; keep credit bounded.
	if b.last.IsZero() {
		b.last = now
		if b.credit > max {
			b.credit = max
		}
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		// Clock moved backwards or called twice in the same instant: no refill.
		return
	}
	b.credit += elapsed * rate
	if b.credit > max {
		b.credit = max
	}
	b.last = now
}

// consumeToken tries to consume `tokens` (usually 1.0).
// Returns true if successful, false if not enough credit.
func consumeToken(b *rateBucket, tokens float64) bool {
	if b == nil {
		return false
	}
	if b.credit+1e-9 < tokens { // small epsilon for float safety
		return false
	}
	b.credit -= tokens
	return true
}
