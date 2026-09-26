package fairorder

import (
	"errors"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		CommitWindow:     10 * time.Second,
		RevealWindow:     10 * time.Second,
		UnrevealedPolicy: UnrevealedFlag,
		CrashPolicy:      CrashResume,
	}
}

func newTestService(t *testing.T, cfg Config) (*Service, *ManualClock) {
	t.Helper()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))
	s, err := NewService(cfg, &MemStore{}, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock
}

// commitTx submits a commitment for (payload, salt) to the current round.
func commitTx(t *testing.T, s *Service, round RoundID, payload []byte, salt Salt) Commitment {
	t.Helper()
	c := ComputeCommitment(round, payload, salt)
	if err := s.Submit(c); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return c
}

// revealTx reveals a previously committed transaction.
func revealTx(t *testing.T, s *Service, c Commitment, payload []byte, salt Salt) {
	t.Helper()
	if err := s.Reveal(c, payload, salt); err != nil {
		t.Fatalf("Reveal: %v", err)
	}
}

func TestCommitRevealFinalizeFlow(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, err := s.OpenRound()
	if err != nil {
		t.Fatalf("OpenRound: %v", err)
	}

	salt := randomSalt(t)
	payload := []byte("tx-1")
	commitTx(t, s, round, payload, salt)

	info, err := s.CurrentRound()
	if err != nil {
		t.Fatalf("CurrentRound: %v", err)
	}
	if info.Phase != PhaseCommit || info.Commits != 1 {
		t.Fatalf("want commit phase with 1 commit, got %v/%d", info.Phase, info.Commits)
	}

	clock.Advance(10 * time.Second) // enter reveal phase
	c := ComputeCommitment(round, payload, salt)
	revealTx(t, s, c, payload, salt)

	clock.Advance(10 * time.Second) // past reveal deadline
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if res.RoundID != round || len(res.Ordered) != 1 || len(res.TimedOut) != 0 {
		t.Fatalf("bad result: %+v", res)
	}
	if string(res.Ordered[0].Payload) != "tx-1" {
		t.Fatalf("bad payload: %q", res.Ordered[0].Payload)
	}

	got, err := s.Result(round)
	if err != nil || got.RoundID != round {
		t.Fatalf("Result: %v %+v", err, got)
	}
	if _, err := s.CurrentRound(); !errors.Is(err, ErrNoActiveRound) {
		t.Fatalf("want ErrNoActiveRound, got %v", err)
	}
}

func TestDuplicateCommitRejected(t *testing.T) {
	s, _ := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	c := commitTx(t, s, round, []byte("tx"), randomSalt(t))
	if err := s.Submit(c); !errors.Is(err, ErrDuplicateCommit) {
		t.Fatalf("want ErrDuplicateCommit, got %v", err)
	}
}

func TestDuplicateRevealRejectedAndIncludedOnce(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	payload := []byte("tx")
	c := commitTx(t, s, round, payload, salt)
	clock.Advance(10 * time.Second)

	revealTx(t, s, c, payload, salt)
	if err := s.Reveal(c, payload, salt); !errors.Is(err, ErrDuplicateReveal) {
		t.Fatalf("want ErrDuplicateReveal, got %v", err)
	}

	clock.Advance(10 * time.Second)
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(res.Ordered) != 1 {
		t.Fatalf("tx must appear exactly once, got %d", len(res.Ordered))
	}
}

func TestMismatchedRevealRejected(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	c := commitTx(t, s, round, []byte("honest"), salt)
	clock.Advance(10 * time.Second)

	if err := s.Reveal(c, []byte("tampered"), salt); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("want ErrCommitmentMismatch, got %v", err)
	}
	if err := s.Reveal(c, []byte("honest"), makeSalt(t, 1)); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("want ErrCommitmentMismatch for wrong salt, got %v", err)
	}
	// The honest reveal still works afterwards.
	revealTx(t, s, c, []byte("honest"), salt)
}

func TestRevealUnknownCommitmentRejected(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	clock.Advance(10 * time.Second)
	ghost := ComputeCommitment(round, []byte("never committed"), randomSalt(t))
	if err := s.Reveal(ghost, []byte("never committed"), randomSalt(t)); !errors.Is(err, ErrUnknownCommitment) {
		t.Fatalf("want ErrUnknownCommitment, got %v", err)
	}
}

func TestUnrevealedCommitExcludedAndFlagged(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	payload := []byte("revealed")
	c := commitTx(t, s, round, payload, salt)
	lazy := commitTx(t, s, round, []byte("never revealed"), randomSalt(t))

	clock.Advance(10 * time.Second)
	revealTx(t, s, c, payload, salt)
	clock.Advance(10 * time.Second)
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(res.Ordered) != 1 {
		t.Fatalf("unrevealed commit must be excluded, got %d txs", len(res.Ordered))
	}
	if len(res.TimedOut) != 1 || res.TimedOut[0] != lazy {
		t.Fatalf("unrevealed commit must be flagged, got %v", res.TimedOut)
	}
}

func TestUnrevealedCommitExcludedSilently(t *testing.T) {
	cfg := testConfig()
	cfg.UnrevealedPolicy = UnrevealedExclude
	s, clock := newTestService(t, cfg)
	round, _ := s.OpenRound()
	commitTx(t, s, round, []byte("never revealed"), randomSalt(t))
	clock.Advance(20 * time.Second)
	res, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(res.Ordered) != 0 || len(res.TimedOut) != 0 {
		t.Fatalf("want empty result, got %+v", res)
	}
}

func TestPhaseBoundaries(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	salt := randomSalt(t)
	payload := []byte("tx")
	c := ComputeCommitment(round, payload, salt)

	// Reveal during commit phase.
	if err := s.Reveal(c, payload, salt); !errors.Is(err, ErrRevealNotStarted) {
		t.Fatalf("want ErrRevealNotStarted, got %v", err)
	}
	// Finalize before reveal deadline.
	if _, err := s.Finalize(); !errors.Is(err, ErrFinalizeTooEarly) {
		t.Fatalf("want ErrFinalizeTooEarly, got %v", err)
	}
	// Commit succeeds inside the commit window.
	if err := s.Submit(c); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	clock.Advance(10 * time.Second)
	// Commit after commit deadline.
	late := ComputeCommitment(round, []byte("late"), randomSalt(t))
	if err := s.Submit(late); !errors.Is(err, ErrCommitPhaseOver) {
		t.Fatalf("want ErrCommitPhaseOver, got %v", err)
	}
	// Reveal exactly at the start of the reveal window works.
	revealTx(t, s, c, payload, salt)

	clock.Advance(10 * time.Second)
	// Reveal after reveal deadline.
	if err := s.Reveal(c, payload, salt); !errors.Is(err, ErrRevealPhaseOver) {
		t.Fatalf("want ErrRevealPhaseOver, got %v", err)
	}
	if _, err := s.Finalize(); err != nil {
		t.Fatalf("Finalize at deadline: %v", err)
	}
}

// TestFrontRunningLateCommit: an attacker who sees the victim's reveal
// cannot squeeze a copycat commitment into the same round.
func TestFrontRunningLateCommit(t *testing.T) {
	s, clock := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	victimSalt := randomSalt(t)
	victimPayload := []byte("victim buys")
	victimCommit := commitTx(t, s, round, victimPayload, victimSalt)

	clock.Advance(10 * time.Second) // reveal phase
	revealTx(t, s, victimCommit, victimPayload, victimSalt)

	// Attacker observes the reveal and tries to front-run in the same round.
	attackerCommit := ComputeCommitment(round, []byte("attacker buys first"), randomSalt(t))
	if err := s.Submit(attackerCommit); !errors.Is(err, ErrCommitPhaseOver) {
		t.Fatalf("front-running commit must be rejected, got %v", err)
	}
	// Attacker tries to replay the victim's reveal.
	if err := s.Reveal(victimCommit, victimPayload, victimSalt); !errors.Is(err, ErrDuplicateReveal) {
		t.Fatalf("replaying victim reveal must be rejected, got %v", err)
	}
	// Attacker cannot open the victim's (already revealed) commitment with
	// a different payload: rejected as a duplicate reveal of that commitment.
	if err := s.Reveal(victimCommit, []byte("attacker buys first"), randomSalt(t)); !errors.Is(err, ErrDuplicateReveal) {
		t.Fatalf("forged reveal must be rejected, got %v", err)
	}
}

// TestRevealTimingDoesNotAffectOrder runs the same round twice with reveals
// in opposite order; the final ordering must be identical.
func TestRevealTimingDoesNotAffectOrder(t *testing.T) {
	run := func(reverse bool) []Commitment {
		cfg := testConfig()
		clock := NewManualClock(time.Unix(1_700_000_000, 0))
		s, err := NewService(cfg, &MemStore{}, clock)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		defer s.Close()
		round, _ := s.OpenRound()

		const n = 16
		commits := make([]Commitment, n)
		payloads := make([][]byte, n)
		salts := make([]Salt, n)
		for i := 0; i < n; i++ {
			payloads[i] = []byte{byte(i)}
			salts[i] = makeSalt(t, byte(i+1))
			commits[i] = commitTx(t, s, round, payloads[i], salts[i])
		}
		clock.Advance(10 * time.Second)
		for k := 0; k < n; k++ {
			i := k
			if reverse {
				i = n - 1 - k
			}
			revealTx(t, s, commits[i], payloads[i], salts[i])
			clock.Advance(100 * time.Millisecond)
		}
		clock.Advance(10 * time.Second)
		res, err := s.Finalize()
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		out := make([]Commitment, len(res.Ordered))
		for i, tx := range res.Ordered {
			out[i] = tx.Commitment
		}
		return out
	}

	forward := run(false)
	backward := run(true)
	if len(forward) != len(backward) {
		t.Fatalf("length mismatch")
	}
	for i := range forward {
		if forward[i] != backward[i] {
			t.Fatalf("position %d: reveal timing changed the order", i)
		}
	}
}

func TestOpenRoundWhileActive(t *testing.T) {
	s, _ := newTestService(t, testConfig())
	if _, err := s.OpenRound(); err != nil {
		t.Fatalf("OpenRound: %v", err)
	}
	if _, err := s.OpenRound(); !errors.Is(err, ErrRoundActive) {
		t.Fatalf("want ErrRoundActive, got %v", err)
	}
}

func TestResultQueries(t *testing.T) {
	s, _ := newTestService(t, testConfig())
	round, _ := s.OpenRound()
	if _, err := s.Result(round); !errors.Is(err, ErrRoundNotFinalized) {
		t.Fatalf("want ErrRoundNotFinalized, got %v", err)
	}
	if _, err := s.Result(round + 99); !errors.Is(err, ErrUnknownRound) {
		t.Fatalf("want ErrUnknownRound, got %v", err)
	}
}

func TestMaxCommits(t *testing.T) {
	cfg := testConfig()
	cfg.MaxCommits = 2
	s, _ := newTestService(t, cfg)
	round, _ := s.OpenRound()
	commitTx(t, s, round, []byte("a"), randomSalt(t))
	commitTx(t, s, round, []byte("b"), randomSalt(t))
	c := ComputeCommitment(round, []byte("c"), randomSalt(t))
	if err := s.Submit(c); !errors.Is(err, ErrTooManyCommits) {
		t.Fatalf("want ErrTooManyCommits, got %v", err)
	}
}

func TestRoundsDoNotInterfere(t *testing.T) {
	s, clock := newTestService(t, testConfig())

	// Round 1 with one tx.
	r1, _ := s.OpenRound()
	salt1 := randomSalt(t)
	c1 := commitTx(t, s, r1, []byte("round-1 tx"), salt1)
	clock.Advance(10 * time.Second)
	revealTx(t, s, c1, []byte("round-1 tx"), salt1)
	clock.Advance(10 * time.Second)
	res1, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Round 2: replaying round 1's reveal must fail (commitment binds round).
	r2, _ := s.OpenRound()
	if r2 == r1 {
		t.Fatal("round ids must increase")
	}
	clock.Advance(10 * time.Second)
	if err := s.Reveal(c1, []byte("round-1 tx"), salt1); !errors.Is(err, ErrUnknownCommitment) {
		t.Fatalf("cross-round replay must fail, got %v", err)
	}
	clock.Advance(10 * time.Second)
	if _, err := s.Finalize(); err != nil {
		t.Fatalf("Finalize round 2: %v", err)
	}

	// Round 1 result still intact.
	got, err := s.Result(r1)
	if err != nil || len(got.Ordered) != 1 {
		t.Fatalf("round 1 result corrupted: %v %+v", err, got)
	}
	if len(res1.Ordered) != 1 {
		t.Fatalf("round 1 result changed")
	}
}
