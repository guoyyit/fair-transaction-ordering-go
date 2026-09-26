package fairorder

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	base := testConfig()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero commit window", func(c *Config) { c.CommitWindow = 0 }},
		{"negative commit window", func(c *Config) { c.CommitWindow = -time.Second }},
		{"zero reveal window", func(c *Config) { c.RevealWindow = 0 }},
		{"negative reveal window", func(c *Config) { c.RevealWindow = -time.Second }},
		{"bad unrevealed policy", func(c *Config) { c.UnrevealedPolicy = UnrevealedPolicy(9) }},
		{"bad crash policy", func(c *Config) { c.CrashPolicy = CrashPolicy(9) }},
		{"negative max commits", func(c *Config) { c.MaxCommits = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
			if _, err := NewService(cfg, &MemStore{}, NewManualClock(time.Now())); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewService: want ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestNewServiceNilDeps(t *testing.T) {
	cfg := testConfig()
	clock := NewManualClock(time.Now())
	if _, err := NewService(cfg, nil, clock); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil store: want ErrInvalidConfig, got %v", err)
	}
	if _, err := NewService(cfg, &MemStore{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil clock: want ErrInvalidConfig, got %v", err)
	}
}

func TestEmptyRoundFinalizes(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	clock.Advance(20 * time.Second)
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize empty round: %v", err)
	}
	if res.RoundID != round || len(res.Ordered) != 0 {
		t.Fatalf("bad empty result: %+v", res)
	}
}

func TestCommitBoundaryExact(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	c := ComputeCommitment(round, []byte("x"), randomSalt(t))
	// One nanosecond before the deadline: still accepted.
	clock.Advance(10*time.Second - time.Nanosecond)
	if err := s.Submit(c); err != nil {
		t.Fatalf("Submit just before deadline: %v", err)
	}
	// Exactly at the deadline: rejected.
	clock.Advance(time.Nanosecond)
	c2 := ComputeCommitment(round, []byte("y"), randomSalt(t))
	if err := s.Submit(c2); !errors.Is(err, ErrCommitPhaseOver) {
		t.Fatalf("want ErrCommitPhaseOver at exact deadline, got %v", err)
	}
}

func TestRevealBoundaryExact(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	payload := []byte("x")
	c := commitTx(t, s, round, payload, salt)
	// One nanosecond before reveal deadline: still accepted.
	clock.Advance(20*time.Second - time.Nanosecond)
	revealTx(t, s, c, payload, salt)
	// Exactly at the deadline: rejected.
	clock.Advance(time.Nanosecond)
	if err := s.Reveal(c, payload, salt); !errors.Is(err, ErrRevealPhaseOver) {
		t.Fatalf("want ErrRevealPhaseOver at exact deadline, got %v", err)
	}
}

func TestEmptyPayloadRejected(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	c := ComputeCommitment(round, nil, salt)
	if err := s.Submit(c); err != nil {
		t.Fatalf("Submit commitment of empty payload is allowed: %v", err)
	}
	clock.Advance(10 * time.Second)
	if err := s.Reveal(c, nil, salt); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("want ErrEmptyPayload, got %v", err)
	}
}

func TestServiceClosed(t *testing.T) {
	cfg := testConfig()
	clock := NewManualClock(time.Now())
	s, err := NewService(cfg, &MemStore{}, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s.OpenRound()
	c := ComputeCommitment(round, []byte("x"), randomSalt(t))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Submit(c); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("want ErrServiceClosed, got %v", err)
	}
	if _, err := s.OpenRound(); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("want ErrServiceClosed, got %v", err)
	}
	// Double close is a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestSequentialRounds(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	var ids []RoundID
	for i := 0; i < 3; i++ {
		round, err := s.OpenRound()
		if err != nil {
			t.Fatalf("OpenRound %d: %v", i, err)
		}
		ids = append(ids, round)
		salt := randomSalt(t)
		payload := []byte{byte(i)}
		c := commitTx(t, s, round, payload, salt)
		clock.Advance(10 * time.Second)
		revealTx(t, s, c, payload, salt)
		clock.Advance(10 * time.Second)
		if _, err := s.Finalize(); err != nil {
			t.Fatalf("Finalize %d: %v", i, err)
		}
	}
	for i, id := range ids {
		if id != RoundID(i+1) {
			t.Fatalf("round ids must be sequential from 1, got %v", ids)
		}
		res, err := s.Result(id)
		if err != nil || len(res.Ordered) != 1 {
			t.Fatalf("round %d result missing: %v", id, err)
		}
	}
}
