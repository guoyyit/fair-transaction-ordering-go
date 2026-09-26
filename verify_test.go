package fairorder

import (
	"errors"
	"testing"
	"time"
)

// finalizeRoundWith runs a full round with n transactions and returns the
// round id and result.
func finalizeRoundWith(t *testing.T, n int) (RoundID, RoundResult) {
	t.Helper()
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	type entry struct {
		c       Commitment
		payload []byte
		salt    Salt
	}
	entries := make([]entry, n)
	for i := 0; i < n; i++ {
		payload := []byte{byte(i * 7)}
		salt := randomSalt(t)
		entries[i] = entry{commitTx(t, s, round, payload, salt), payload, salt}
	}
	// One commit is never revealed (flagged as timed out).
	commitTx(t, s, round, []byte("lazy"), randomSalt(t))
	clock.Advance(10 * time.Second)
	for _, e := range entries {
		revealTx(t, s, e.c, e.payload, e.salt)
	}
	clock.Advance(10 * time.Second)
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return round, res
}

func TestVerifyResultAcceptsHonestResult(t *testing.T) {
	round, res := finalizeRoundWith(t, 32)
	if err := VerifyResult(round, res); err != nil {
		t.Fatalf("honest result rejected: %v", err)
	}
}

func TestVerifyResultDetectsTampering(t *testing.T) {
	round, res := finalizeRoundWith(t, 8)

	// Wrong round id.
	bad := res
	if err := VerifyResult(round+1, bad); !errors.Is(err, ErrResultRoundMismatch) {
		t.Fatalf("want ErrResultRoundMismatch, got %v", err)
	}

	// Swapped adjacent positions (almost certainly breaks the sort).
	bad = res
	bad.Ordered = append([]RevealedTx(nil), res.Ordered...)
	bad.Ordered[0], bad.Ordered[1] = bad.Ordered[1], bad.Ordered[0]
	if err := VerifyResult(round, bad); !errors.Is(err, ErrResultBadOrder) {
		t.Fatalf("want ErrResultBadOrder, got %v", err)
	}

	// Duplicated transaction.
	bad = res
	bad.Ordered = append(append([]RevealedTx(nil), res.Ordered...), res.Ordered[0])
	if err := VerifyResult(round, bad); !errors.Is(err, ErrResultDuplicate) {
		t.Fatalf("want ErrResultDuplicate, got %v", err)
	}

	// Tampered payload no longer opens the commitment.
	bad = res
	bad.Ordered = append([]RevealedTx(nil), res.Ordered...)
	bad.Ordered[0].Payload = []byte("forged")
	if err := VerifyResult(round, bad); !errors.Is(err, ErrResultBadReveal) {
		t.Fatalf("want ErrResultBadReveal, got %v", err)
	}

	// A commitment both ordered and timed out.
	bad = res
	bad.TimedOut = append(append([]Commitment(nil), res.TimedOut...), res.Ordered[0].Commitment)
	if err := VerifyResult(round, bad); !errors.Is(err, ErrResultOverlap) {
		t.Fatalf("want ErrResultOverlap, got %v", err)
	}
}

// TestResultStableAcrossRecoveryAndTime: the finalized ordering is a pure
// function of the revealed set, so re-deriving it later (e.g. after a
// restart) yields byte-identical output.
func TestResultStableAcrossRecoveryAndTime(t *testing.T) {
	cfg := testConfig()
	store := &MemStore{}
	clock := NewManualClock(time.Unix(1_700_000_000, 0))
	s1, err := NewService(cfg, store, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	const n = 24
	type entry struct {
		c       Commitment
		payload []byte
		salt    Salt
	}
	entries := make([]entry, n)
	for i := 0; i < n; i++ {
		payload := []byte{byte(i), byte(i >> 8), 0xAB}
		salt := randomSalt(t)
		entries[i] = entry{commitTx(t, s1, round, payload, salt), payload, salt}
	}
	clock.Advance(10 * time.Second)
	for _, e := range entries {
		revealTx(t, s1, e.c, e.payload, e.salt)
	}
	clock.Advance(10 * time.Second)
	res1, err := s1.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	s1.Close()

	// Restart much later; the persisted result must be identical.
	clock.Advance(24 * time.Hour)
	s2 := reopen(t, cfg, store, clock)
	res2, err := s2.Result(round)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(res1.Ordered) != len(res2.Ordered) {
		t.Fatalf("length changed across restart")
	}
	for i := range res1.Ordered {
		if res1.Ordered[i].Commitment != res2.Ordered[i].Commitment {
			t.Fatalf("position %d changed across restart", i)
		}
	}
	if err := VerifyResult(round, res2); err != nil {
		t.Fatalf("recovered result fails verification: %v", err)
	}
}
