package fairorder

import (
	"sync"
	"testing"
	"time"
)

// runConcurrentRound executes one round where commits and reveals arrive
// from many goroutines in non-deterministic order, and returns the final
// ordering. The barrier channel staggers goroutine start to maximize
// interleaving.
func runConcurrentRound(t *testing.T, n int) []Commitment {
	t.Helper()
	cfg := testConfig()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))
	s, err := NewService(cfg, &MemStore{}, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer s.Close()

	round, err := s.OpenRound()
	if err != nil {
		t.Fatalf("OpenRound: %v", err)
	}

	commits := make([]Commitment, n)
	payloads := make([][]byte, n)
	salts := make([]Salt, n)
	for i := 0; i < n; i++ {
		payloads[i] = []byte{byte(i), byte(i >> 8)}
		salts[i] = makeSalt(t, byte(i+1))
		commits[i] = ComputeCommitment(round, payloads[i], salts[i])
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.Submit(commits[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	clock.Advance(10 * time.Second)

	start2 := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start2
			errs[i] = s.Reveal(commits[i], payloads[i], salts[i])
		}(i)
	}
	close(start2)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Reveal %d: %v", i, err)
		}
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

// TestConcurrentOrderingDeterministic: the same set of transactions
// committed and revealed under chaotic concurrency must always yield the
// same final order.
func TestConcurrentOrderingDeterministic(t *testing.T) {
	const n = 64
	reference := runConcurrentRound(t, n)
	if len(reference) != n {
		t.Fatalf("want %d txs, got %d", n, len(reference))
	}
	for trial := 0; trial < 5; trial++ {
		got := runConcurrentRound(t, n)
		for i := range reference {
			if got[i] != reference[i] {
				t.Fatalf("trial %d position %d: order diverged under concurrency", trial, i)
			}
		}
	}
}

// TestConcurrentRoundsIsolated: back-to-back rounds under concurrent load
// must not leak transactions into each other.
func TestConcurrentRoundsIsolated(t *testing.T) {
	cfg := testConfig()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))
	s, err := NewService(cfg, &MemStore{}, clock)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer s.Close()

	const rounds = 5
	const perRound = 16
	seen := make(map[Commitment]RoundID)

	for r := 0; r < rounds; r++ {
		round, err := s.OpenRound()
		if err != nil {
			t.Fatalf("OpenRound %d: %v", r, err)
		}
		commits := make([]Commitment, perRound)
		payloads := make([][]byte, perRound)
		salts := make([]Salt, perRound)
		var wg sync.WaitGroup
		for i := 0; i < perRound; i++ {
			payloads[i] = []byte{byte(r), byte(i)}
			salts[i] = makeSalt(t, byte(r*perRound+i+1))
			commits[i] = ComputeCommitment(round, payloads[i], salts[i])
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := s.Submit(commits[i]); err != nil {
					t.Errorf("Submit: %v", err)
				}
			}(i)
		}
		wg.Wait()
		clock.Advance(10 * time.Second)
		for i := 0; i < perRound; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := s.Reveal(commits[i], payloads[i], salts[i]); err != nil {
					t.Errorf("Reveal: %v", err)
				}
			}(i)
		}
		wg.Wait()
		clock.Advance(10 * time.Second)
		res, err := s.Finalize()
		if err != nil {
			t.Fatalf("Finalize %d: %v", r, err)
		}
		if len(res.Ordered) != perRound {
			t.Fatalf("round %d: want %d txs, got %d", r, perRound, len(res.Ordered))
		}
		for _, tx := range res.Ordered {
			if prev, dup := seen[tx.Commitment]; dup {
				t.Fatalf("commitment appears in rounds %d and %d", prev, r)
			}
			seen[tx.Commitment] = RoundID(r)
			// Payload must belong to this round (first byte is round index).
			if int(tx.Payload[0]) != r {
				t.Fatalf("tx from round %d leaked into round %d", tx.Payload[0], r)
			}
		}
	}
}
