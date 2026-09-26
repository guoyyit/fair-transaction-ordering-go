package fairorder

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "data")
}

func openService(t *testing.T, dir string, clock *fakeClock) *Service {
	t.Helper()
	svc, err := NewService(ServiceConfig{DataDir: dir, Now: clock.now, TickInterval: time.Hour})
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	return svc
}

// 在提交阶段后崩溃：已确认提交必须恢复，轮次可继续走完且不重复纳入。
func TestCrashAfterCommit(t *testing.T) {
	dir := newDir(t)
	clock := &fakeClock{t: time.Unix(5000, 0)}

	svc := openService(t, dir, clock)
	r, _ := svc.CreateRound(testConfig("rec1", clock.t, PolicyExcludeUnrevealed))
	mustCommit(t, r, "a", 1, []byte("sa"), []byte("pa"))
	mustCommit(t, r, "b", 1, []byte("sb"), []byte("pb"))
	_ = svc.Close() // 模拟崩溃：仍在提交阶段

	svc2 := openService(t, dir, clock) // 时钟未推进，仍处于提交阶段
	defer svc2.Close()
	r2, err := svc2.GetRound("rec1")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Info().CommitCount != 2 {
		t.Fatalf("recovered commits = %d, want 2", r2.Info().CommitCount)
	}
	// 重复提交不得再次纳入。
	if err := r2.SubmitCommit(CommitRecord{Sender: "a", Nonce: 1, Commitment: MakeCommitment([]byte("sa"), []byte("pa"))}); !errors.Is(err, ErrDuplicateCommit) {
		t.Fatalf("recovered duplicate commit: got %v", err)
	}
	clock.advance(100 * time.Millisecond) // 进入揭示期
	if err := r2.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("sa"), Payload: []byte("pa")}); err != nil {
		t.Fatal(err)
	}
	if err := r2.SubmitReveal(RevealRecord{Sender: "b", Nonce: 1, Salt: []byte("sb"), Payload: []byte("pb")}); err != nil {
		t.Fatal(err)
	}
	clock.advance(100 * time.Millisecond)
	res, err := r2.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 2 {
		t.Fatalf("tx count = %d, want 2", len(res.Transactions))
	}
}

// 在揭示阶段后、最终完成前崩溃：已确认揭示必须恢复，超时策略照常生效。
func TestCrashAfterRevealExcludePolicy(t *testing.T) {
	dir := newDir(t)
	clock := &fakeClock{t: time.Unix(6000, 0)}

	svc := openService(t, dir, clock)
	r, _ := svc.CreateRound(testConfig("rec2", clock.t, PolicyExcludeUnrevealed))
	mustCommit(t, r, "a", 1, []byte("sa"), []byte("pa"))
	mustCommit(t, r, "b", 1, []byte("sb"), []byte("pb"))
	clock.advance(100 * time.Millisecond)
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("sa"), Payload: []byte("pa")}); err != nil {
		t.Fatal(err)
	}
	_ = svc.Close()

	// b 始终未揭示；重启时揭示截止时间已过，按排除策略完成。
	clock.advance(200 * time.Millisecond)
	svc2 := openService(t, dir, clock)
	defer svc2.Close()
	r2, err := svc2.GetRound("rec2")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status() != StatusFinalized {
		t.Fatalf("status = %s, want finalized", r2.Status())
	}
	res, err := r2.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 1 || res.Transactions[0].Sender != "a" {
		t.Fatalf("txs = %+v", res.Transactions)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Sender != "b" {
		t.Fatalf("excluded = %+v", res.Excluded)
	}
}

// 揭示截止后以 abort 策略终止的轮次，崩溃重启后必须仍是终止态，结果不丢也不重放成完成。
func TestCrashAbortedRoundStaysAborted(t *testing.T) {
	dir := newDir(t)
	clock := &fakeClock{t: time.Unix(7000, 0)}

	svc := openService(t, dir, clock)
	r, _ := svc.CreateRound(testConfig("rec3", clock.t, PolicyAbort))
	mustCommit(t, r, "a", 1, []byte("sa"), []byte("pa"))
	mustCommit(t, r, "b", 1, []byte("sb"), []byte("pb"))
	clock.advance(100 * time.Millisecond)
	_ = r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("sa"), Payload: []byte("pa")})
	clock.advance(100 * time.Millisecond)
	if _, err := r.Result(); !errors.Is(err, ErrRoundAborted) {
		t.Fatalf("expect abort, got %v", err)
	}
	_ = svc.Close()

	clock.advance(time.Hour)
	svc2 := openService(t, dir, clock)
	defer svc2.Close()
	r2, _ := svc2.GetRound("rec3")
	if r2.Status() != StatusAborted {
		t.Fatalf("status after restart = %s, want aborted", r2.Status())
	}
	if _, err := r2.Result(); !errors.Is(err, ErrRoundAborted) {
		t.Fatalf("result after restart: got %v", err)
	}
	// 终态轮次拒绝继续提交/揭示，避免重复纳入。
	if err := r2.SubmitReveal(RevealRecord{Sender: "b", Nonce: 1, Salt: []byte("sb"), Payload: []byte("pb")}); !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("reveal on aborted round: got %v", err)
	}
}

// 已完成轮次崩溃重启后，已确认结果必须原样可读，且确定性可复算。
func TestCrashFinalizedResultPreserved(t *testing.T) {
	dir := newDir(t)
	clock := &fakeClock{t: time.Unix(8000, 0)}

	svc := openService(t, dir, clock)
	r, _ := svc.CreateRound(testConfig("rec4", clock.t, PolicyExcludeUnrevealed))
	mustCommit(t, r, "a", 1, []byte("sa"), []byte("pa"))
	mustCommit(t, r, "b", 1, []byte("sb"), []byte("pb"))
	clock.advance(100 * time.Millisecond)
	_ = r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("sa"), Payload: []byte("pa")})
	_ = r.SubmitReveal(RevealRecord{Sender: "b", Nonce: 1, Salt: []byte("sb"), Payload: []byte("pb")})
	clock.advance(100 * time.Millisecond)
	before, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Close()

	clock.advance(time.Hour)
	svc2 := openService(t, dir, clock)
	defer svc2.Close()
	r2, _ := svc2.GetRound("rec4")
	if r2.Status() != StatusFinalized {
		t.Fatalf("status = %s", r2.Status())
	}
	after, err := r2.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Transactions) != len(before.Transactions) {
		t.Fatalf("tx count changed: %d vs %d", len(after.Transactions), len(before.Transactions))
	}
	for i := range before.Transactions {
		if before.Transactions[i].Sender != after.Transactions[i].Sender ||
			string(before.Transactions[i].OrderKey) != string(after.Transactions[i].OrderKey) {
			t.Fatalf("result changed across restart at index %d", i)
		}
	}
}

// 多轮次同时存在时崩溃，恢复后互不串扰。
func TestCrashMultipleRoundsRestored(t *testing.T) {
	dir := newDir(t)
	clock := &fakeClock{t: time.Unix(9000, 0)}
	svc := openService(t, dir, clock)
	_, _ = svc.CreateRound(testConfig("m1", clock.t, PolicyExcludeUnrevealed))
	clock.advance(time.Millisecond)
	_, _ = svc.CreateRound(testConfig("m2", clock.t, PolicyExcludeUnrevealed))
	_ = svc.Close()

	svc2 := openService(t, dir, clock)
	defer svc2.Close()
	ids := svc2.ListRounds()
	if len(ids) != 2 || ids[0] != RoundID("m1") || ids[1] != RoundID("m2") {
		t.Fatalf("restored rounds = %v", ids)
	}
}
