package fairorder

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeClock 是可手动推进的虚拟时钟。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestService(t *testing.T, clock *fakeClock) *Service {
	t.Helper()
	svc, err := NewService(ServiceConfig{Now: clock.now, TickInterval: time.Hour})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func testConfig(id RoundID, start time.Time, policy TimeoutPolicy) RoundConfig {
	return RoundConfig{
		ID:             id,
		CommitDeadline: start.Add(100 * time.Millisecond),
		RevealDeadline: start.Add(200 * time.Millisecond),
		TimeoutPolicy:  policy,
	}
}

func mustCommit(t *testing.T, r *Round, sender string, nonce uint64, salt, payload []byte) {
	t.Helper()
	err := r.SubmitCommit(CommitRecord{
		Sender:     sender,
		Nonce:      nonce,
		Commitment: MakeCommitment(salt, payload),
	})
	if err != nil {
		t.Fatalf("SubmitCommit(%s,%d): %v", sender, nonce, err)
	}
}

func TestHappyPathOrdering(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, err := svc.CreateRound(testConfig("r1", clock.t, PolicyExcludeUnrevealed))
	if err != nil {
		t.Fatal(err)
	}

	type party struct {
		sender  string
		nonce   uint64
		salt    []byte
		payload []byte
	}
	parties := []party{
		{"alice", 1, []byte("salt-a"), []byte("pay-to-carol-10")},
		{"bob", 2, []byte("salt-b"), []byte("pay-to-dave-20")},
		{"carol", 3, []byte("salt-c"), []byte("pay-to-erin-30")},
	}
	for _, p := range parties {
		mustCommit(t, r, p.sender, p.nonce, p.salt, p.payload)
	}

	clock.advance(100 * time.Millisecond) // 进入揭示阶段
	for _, p := range parties {
		if err := r.SubmitReveal(RevealRecord{Sender: p.sender, Nonce: p.nonce, Salt: p.salt, Payload: p.payload}); err != nil {
			t.Fatalf("SubmitReveal(%s): %v", p.sender, err)
		}
	}

	clock.advance(100 * time.Millisecond) // 到达揭示截止
	res, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(res.Transactions) != 3 {
		t.Fatalf("tx count = %d, want 3", len(res.Transactions))
	}
	if len(res.Excluded) != 0 {
		t.Fatalf("excluded = %v, want none", res.Excluded)
	}

	// 结果必须等于独立按公开规则复算的顺序。
	for i := 1; i < len(res.Transactions); i++ {
		if string(res.Transactions[i-1].OrderKey) > string(res.Transactions[i].OrderKey) {
			t.Fatal("order not sorted by order key")
		}
	}
	// 独立校验每个条目的承诺与排序键。
	saltBySender := make(map[string][]byte, len(parties))
	for _, p := range parties {
		saltBySender[p.sender] = p.salt
	}
	for _, tx := range res.Transactions {
		if !VerifyCommitment(tx.Commitment, saltBySender[tx.Sender], tx.Payload) {
			t.Fatalf("commitment verification failed for %s", tx.Sender)
		}
		if string(OrderKey(tx.Payload)) != string(tx.OrderKey) {
			t.Fatalf("order key mismatch for %s", tx.Sender)
		}
	}
}

func TestPhaseGating(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("g1", clock.t, PolicyExcludeUnrevealed))
	salt, payload := []byte("s"), []byte("p")

	// 提交阶段不能揭示。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: salt, Payload: payload}); !errors.Is(err, ErrRevealNotOpen) {
		t.Fatalf("reveal in commit phase: got %v", err)
	}

	mustCommit(t, r, "a", 1, salt, payload)
	clock.advance(100 * time.Millisecond)

	// 揭示阶段不能再提交。
	if err := r.SubmitCommit(CommitRecord{Sender: "b", Nonce: 1, Commitment: MakeCommitment([]byte("x"), []byte("y"))}); !errors.Is(err, ErrCommitPhaseClosed) {
		t.Fatalf("commit in reveal phase: got %v", err)
	}

	clock.advance(100 * time.Millisecond)
	// 终态后拒绝一切提交/揭示。
	if err := r.SubmitCommit(CommitRecord{Sender: "c", Nonce: 1, Commitment: MakeCommitment(nil, nil)}); !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("commit after finalize: got %v", err)
	}
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: salt, Payload: payload}); !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("reveal after finalize: got %v", err)
	}
}

func TestDuplicateAndInvalidReveal(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("d1", clock.t, PolicyExcludeUnrevealed))
	salt, payload := []byte("salt"), []byte("payload")
	mustCommit(t, r, "a", 1, salt, payload)

	// 同一身份重复提交被拒绝。
	err := r.SubmitCommit(CommitRecord{Sender: "a", Nonce: 1, Commitment: MakeCommitment(salt, payload)})
	if !errors.Is(err, ErrDuplicateCommit) {
		t.Fatalf("duplicate commit: got %v", err)
	}

	clock.advance(100 * time.Millisecond)
	// 无对应承诺的揭示被拒绝。
	if err := r.SubmitReveal(RevealRecord{Sender: "ghost", Nonce: 9, Salt: salt, Payload: payload}); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("reveal without commit: got %v", err)
	}
	// 盐值错误，承诺不匹配，必须拒绝。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("wrong"), Payload: payload}); !errors.Is(err, ErrInvalidCommitment) {
		t.Fatalf("invalid reveal: got %v", err)
	}
	// 原文被篡改，同样拒绝。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: salt, Payload: []byte("payload-tampered")}); !errors.Is(err, ErrInvalidCommitment) {
		t.Fatalf("tampered reveal: got %v", err)
	}
	// 正确揭示被接受。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: salt, Payload: payload}); err != nil {
		t.Fatalf("valid reveal: %v", err)
	}
	// 重复揭示被拒绝。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: salt, Payload: payload}); !errors.Is(err, ErrDuplicateReveal) {
		t.Fatalf("duplicate reveal: got %v", err)
	}

	clock.advance(100 * time.Millisecond)
	res, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("tx count = %d, want 1", len(res.Transactions))
	}
}

func TestTimeoutExcludePolicy(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("t1", clock.t, PolicyExcludeUnrevealed))
	mustCommit(t, r, "honest", 1, []byte("s1"), []byte("revealed-tx"))
	mustCommit(t, r, "silent", 1, []byte("s2"), []byte("never-revealed"))
	clock.advance(100 * time.Millisecond)
	if err := r.SubmitReveal(RevealRecord{Sender: "honest", Nonce: 1, Salt: []byte("s1"), Payload: []byte("revealed-tx")}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	res, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if r.Status() != StatusFinalized {
		t.Fatalf("status = %s, want finalized", r.Status())
	}
	if len(res.Transactions) != 1 || res.Transactions[0].Sender != "honest" {
		t.Fatalf("transactions = %+v", res.Transactions)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Sender != "silent" || res.Excluded[0].Reason != ReasonUnrevealed {
		t.Fatalf("excluded = %+v", res.Excluded)
	}
}

func TestTimeoutAbortPolicy(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("t2", clock.t, PolicyAbort))
	mustCommit(t, r, "honest", 1, []byte("s1"), []byte("revealed-tx"))
	mustCommit(t, r, "silent", 1, []byte("s2"), []byte("never-revealed"))
	clock.advance(100 * time.Millisecond)
	if err := r.SubmitReveal(RevealRecord{Sender: "honest", Nonce: 1, Salt: []byte("s1"), Payload: []byte("revealed-tx")}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	if _, err := r.Result(); !errors.Is(err, ErrRoundAborted) {
		t.Fatalf("Result err = %v, want ErrRoundAborted", err)
	}
	if r.Status() != StatusAborted {
		t.Fatalf("status = %s, want aborted", r.Status())
	}
}

func TestAbortPolicyAllRevealedStillFinalizes(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("t3", clock.t, PolicyAbort))
	mustCommit(t, r, "a", 1, []byte("s"), []byte("p"))
	clock.advance(100 * time.Millisecond)
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("s"), Payload: []byte("p")}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	res, err := r.Result()
	if err != nil {
		t.Fatalf("all revealed under abort policy should finalize, got %v", err)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("tx count = %d", len(res.Transactions))
	}
}

func TestRoundIsolation(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r1, _ := svc.CreateRound(testConfig("iso-1", clock.t, PolicyExcludeUnrevealed))
	clock.advance(1 * time.Millisecond)
	r2, _ := svc.CreateRound(testConfig("iso-2", clock.t, PolicyExcludeUnrevealed))
	mustCommit(t, r1, "a", 1, []byte("s"), []byte("tx-r1"))
	mustCommit(t, r2, "a", 1, []byte("s"), []byte("tx-r2"))
	clock.advance(100 * time.Millisecond)
	// r1 已经过提交截止，r2 也进入揭示阶段（截止时间仅相差 1ms）。
	if err := r1.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("s"), Payload: []byte("tx-r1")}); err != nil {
		t.Fatal(err)
	}
	if err := r2.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("s"), Payload: []byte("tx-r2")}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	res1, err := r1.Result()
	if err != nil {
		t.Fatal(err)
	}
	res2, err := r2.Result()
	if err != nil {
		t.Fatal(err)
	}
	if string(res1.Transactions[0].Payload) != "tx-r1" || string(res2.Transactions[0].Payload) != "tx-r2" {
		t.Fatalf("round crosstalk: %s vs %s", res1.Transactions[0].Payload, res2.Transactions[0].Payload)
	}
	if _, err := svc.GetRound("iso-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetRound("missing"); !errors.Is(err, ErrRoundNotFound) {
		t.Fatalf("missing round: got %v", err)
	}
}

func TestInvalidConfig(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	base := testConfig("cfg", clock.t, PolicyExcludeUnrevealed)
	if _, err := svc.CreateRound(RoundConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty id: got %v", err)
	}
	bad := base
	bad.ID = "cfg2"
	bad.RevealDeadline = bad.CommitDeadline
	if _, err := svc.CreateRound(bad); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("deadline order: got %v", err)
	}
	if _, err := svc.CreateRound(base); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRound(base); !errors.Is(err, ErrRoundExists) {
		t.Fatalf("duplicate round: got %v", err)
	}
}

func TestResultBeforeFinalize(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	svc := newTestService(t, clock)
	r, _ := svc.CreateRound(testConfig("early", clock.t, PolicyExcludeUnrevealed))
	if _, err := r.Result(); !errors.Is(err, ErrRoundNotFinalized) {
		t.Fatalf("early result: got %v", err)
	}
	if ci := r.Info(); ci.Status != StatusCommitting {
		t.Fatalf("info status = %s", ci.Status)
	}
	_ = fmt.Sprint("")
}
