package fairorder

import (
	"sync"
	"time"
)

// Clock abstracts time so tests can control round deadlines.
type Clock interface {
	Now() time.Time
}

// RealClock uses the system clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// ManualClock is a deterministic clock for tests.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock creates a ManualClock starting at t.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t}
}

// Now returns the current manual time.
func (m *ManualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Advance moves the clock forward by d.
func (m *ManualClock) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}

// Set moves the clock to t.
func (m *ManualClock) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = t
}
