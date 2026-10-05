package app

import "time"

// Clock is time as the supervisor sees it; tests drive a fake one.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// After waits for d.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// backoff doubles the reconnect delay from base up to max.
type backoff struct {
	base, max, next time.Duration
}

func newBackoff(base, max time.Duration) *backoff { return &backoff{base: base, max: max, next: base} }

// Next returns the delay to wait now and doubles the following one, capped.
func (b *backoff) Next() time.Duration {
	d := b.next
	b.next *= 2
	if b.next > b.max {
		b.next = b.max
	}
	return d
}

// Reset starts again from base after a successful connection.
func (b *backoff) Reset() { b.next = b.base }
