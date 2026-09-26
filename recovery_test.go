package fairorder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reopen simulates a crash + restart over the same store and clock.
func reopen(t *testing.T, cfg Config, store Store, clock Clock) *Service {
	t.Helper()
	s, err := NewService(cfg, store, clock)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCrashRecoveryResume(t *testing.T) {
	cfg := testConfig()
	store := &MemStore{}
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	s1, err := NewService(cfg, store, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	salt1, salt2 := randomSalt(t), randomSalt(t)
	c1 := commitTx(t, s1, round, []byte("tx-1"), salt1)
	c2 := commitTx(t, s1, round, []byte("tx-2"), salt2)
	s1.Close() // simulate crash mid commit-phase

	// Recover: round must still be active with all state intact.
	s2 := reopen(t, cfg, store, clock)
	info, err := s2.CurrentRound()
	if err != nil {
		t.Fatalf("CurrentRound after recovery: %v", err)
	}
	if info.ID != round || info.Commits != 2 || info.Reveals != 0 {
		t.Fatalf("lost state across crash: %+v", info)
	}
	// Duplicate commit must still be rejected after recovery.
	if err := s2.Submit(c2); !errors.Is(err, ErrDuplicateCommit) {
		t.Fatalf("want ErrDuplicateCommit after recovery, got %v", err)
	}
	// Move to reveal phase, crash again, recover once more.
	clock.Advance(10 * time.Second)
	revealTx(t, s2, c1, []byte("tx-1"), salt1)
	s2.Close()
	s2 = reopen(t, cfg, store, clock)
	info, err = s2.CurrentRound()
	if err != nil {
		t.Fatalf("CurrentRound after second recovery: %v", err)
	}
	if info.Reveals != 1 {
		t.Fatalf("lost reveal across crash: %+v", info)
	}
	// Reveal the second tx and finalize.
	revealTx(t, s2, c2, []byte("tx-2"), salt2)
	clock.Advance(10 * time.Second)
	res, err := s2.Finalize()
	if err != nil {
		t.Fatalf("Finalize after recovery: %v", err)
	}
	if len(res.Ordered) != 2 {
		t.Fatalf("want 2 txs after recovery, got %d", len(res.Ordered))
	}

	// Crash again after finalize: the result must survive unchanged.
	s3 := reopen(t, cfg, store, clock)
	got, err := s3.Result(round)
	if err != nil {
		t.Fatalf("Result after second recovery: %v", err)
	}
	if len(got.Ordered) != 2 || got.FinalizedAt != res.FinalizedAt {
		t.Fatalf("finalized result lost or altered across crash")
	}
	for i := range res.Ordered {
		if got.Ordered[i].Commitment != res.Ordered[i].Commitment {
			t.Fatalf("position %d: order changed across crash", i)
		}
	}
}

func TestCrashRecoveryFinalizePolicy(t *testing.T) {
	cfg := testConfig()
	cfg.CrashPolicy = CrashFinalize
	store := &MemStore{}
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	s1, err := NewService(cfg, store, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	salt := randomSalt(t)
	c := commitTx(t, s1, round, []byte("tx"), salt)
	commitTx(t, s1, round, []byte("unrevealed"), randomSalt(t))
	clock.Advance(10 * time.Second)
	revealTx(t, s1, c, []byte("tx"), salt)
	s1.Close() // crash before finalize

	s2 := reopen(t, cfg, store, clock)
	res, err := s2.Result(round)
	if err != nil {
		t.Fatalf("round must be finalized on recovery: %v", err)
	}
	if len(res.Ordered) != 1 || len(res.TimedOut) != 1 {
		t.Fatalf("bad recovered result: %+v", res)
	}
	if _, err := s2.CurrentRound(); !errors.Is(err, ErrNoActiveRound) {
		t.Fatalf("want no active round, got %v", err)
	}
}

func TestCrashRecoveryDiscardPolicy(t *testing.T) {
	cfg := testConfig()
	cfg.CrashPolicy = CrashDiscard
	store := &MemStore{}
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	s1, err := NewService(cfg, store, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	salt := randomSalt(t)
	c := commitTx(t, s1, round, []byte("tx"), salt)
	clock.Advance(10 * time.Second)
	revealTx(t, s1, c, []byte("tx"), salt)
	s1.Close() // crash

	s2 := reopen(t, cfg, store, clock)
	if _, err := s2.Result(round); !errors.Is(err, ErrRoundDiscarded) {
		t.Fatalf("want ErrRoundDiscarded, got %v", err)
	}
	// A fresh round can start; its id must not collide.
	r2, err := s2.OpenRound()
	if err != nil {
		t.Fatalf("OpenRound after discard: %v", err)
	}
	if r2 <= round {
		t.Fatalf("round id reused: %d <= %d", r2, round)
	}
}

func TestFileStoreCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	cfg := testConfig()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	fs1, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1, err := NewService(cfg, fs1, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	salt := randomSalt(t)
	c := commitTx(t, s1, round, []byte("durable tx"), salt)
	clock.Advance(10 * time.Second)
	revealTx(t, s1, c, []byte("durable tx"), salt)
	clock.Advance(10 * time.Second)
	res, err := s1.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	s1.Close()

	// Reopen from disk.
	fs2, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	s2, err := NewService(cfg, fs2, clock)
	if err != nil {
		t.Fatalf("recover from file: %v", err)
	}
	defer s2.Close()
	got, err := s2.Result(round)
	if err != nil {
		t.Fatalf("Result after file recovery: %v", err)
	}
	if len(got.Ordered) != 1 || got.Ordered[0].Commitment != res.Ordered[0].Commitment {
		t.Fatalf("file recovery lost finalized result")
	}
}

func TestFileStoreToleratesTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	cfg := testConfig()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	fs1, _ := OpenFileStore(path)
	s1, err := NewService(cfg, fs1, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	round, _ := s1.OpenRound()
	commitTx(t, s1, round, []byte("tx"), randomSalt(t))
	s1.Close()

	// Simulate a torn write: append a partial JSON line.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	if _, err := f.WriteString(`{"type":"committed","round":1,"commitment":"deadbeef`); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	f.Close()

	fs2, _ := OpenFileStore(path)
	s2, err := NewService(cfg, fs2, clock)
	if err != nil {
		t.Fatalf("recovery must tolerate torn tail: %v", err)
	}
	defer s2.Close()
	info, err := s2.CurrentRound()
	if err != nil {
		t.Fatalf("CurrentRound: %v", err)
	}
	if info.Commits != 1 {
		t.Fatalf("torn tail corrupted replay: %+v", info)
	}
}

// TestRecoveredRoundCanExpire: a round whose deadlines passed while the
// process was down can still be finalized after recovery (CrashResume).
func TestRecoveredRoundCanExpire(t *testing.T) {
	cfg := testConfig()
	store := &MemStore{}
	clock := NewManualClock(time.Unix(1_700_000_000, 0))

	s1, _ := NewService(cfg, store, clock)
	round, _ := s1.OpenRound()
	salt := randomSalt(t)
	c := commitTx(t, s1, round, []byte("tx"), salt)
	clock.Advance(10 * time.Second)
	revealTx(t, s1, c, []byte("tx"), salt)
	s1.Close()

	// Process stays down past the reveal deadline.
	clock.Advance(time.Hour)
	s2 := reopen(t, cfg, store, clock)
	// New reveals are rejected (deadline passed)...
	if err := s2.Reveal(c, []byte("tx"), salt); !errors.Is(err, ErrRevealPhaseOver) {
		t.Fatalf("want ErrRevealPhaseOver, got %v", err)
	}
	// ...but the round can still be finalized with what was revealed.
	res, err := s2.Finalize()
	if err != nil {
		t.Fatalf("Finalize expired round: %v", err)
	}
	if len(res.Ordered) != 1 {
		t.Fatalf("want 1 tx, got %d", len(res.Ordered))
	}
}
