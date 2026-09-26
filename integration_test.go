package fairorder

import (
	"context"
	"testing"
	"time"
)

// 使用真实时钟与快速 ticker：轮次必须在截止时间后自动进入终态。
func TestRealClockAutoFinalize(t *testing.T) {
	svc, err := NewService(ServiceConfig{TickInterval: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	now := time.Now()
	cfg := RoundConfig{
		ID:             "live",
		CommitDeadline: now.Add(40 * time.Millisecond),
		RevealDeadline: now.Add(80 * time.Millisecond),
		TimeoutPolicy:  PolicyExcludeUnrevealed,
	}
	r, err := svc.CreateRound(cfg)
	if err != nil {
		t.Fatal(err)
	}
	salt, payload := []byte("s"), []byte("live-tx")
	if err := r.SubmitCommit(CommitRecord{Sender: "a", Nonce: 1, Commitment: MakeCommitment(salt, payload)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := svc.WaitRoundFinalized(ctx, "live")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if status != StatusFinalized {
		t.Fatalf("status = %s, want finalized", status)
	}
	// 唯一的提交未揭示：被排除，顺序为空但轮次正常完成。
	res, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 0 || len(res.Excluded) != 1 {
		t.Fatalf("unexpected result: tx=%d excluded=%d", len(res.Transactions), len(res.Excluded))
	}
}

// 边界语义：截止时刻 t 提交关闭（区间为左闭右开）；恰好在揭示截止点的揭示被拒绝。
func TestDeadlineBoundarySemantics(t *testing.T) {
	base := time.Unix(10000, 0)
	clock := &fakeClock{t: base}
	svc := newTestService(t, clock)
	cfg := RoundConfig{ID: "b", CommitDeadline: base.Add(10), RevealDeadline: base.Add(20), TimeoutPolicy: PolicyExcludeUnrevealed}
	r, err := svc.CreateRound(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 恰好到达提交截止时刻：提交阶段为 [start, commitDeadline)，此刻已关闭。
	clock.advance(10)
	if err := r.SubmitCommit(CommitRecord{Sender: "a", Nonce: 1, Commitment: MakeCommitment([]byte("s"), []byte("p"))}); err != ErrCommitPhaseClosed {
		t.Fatalf("commit exactly at deadline: got %v", err)
	}
	// 揭示阶段起点提交已关但揭示开放。
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1}); err != ErrCommitNotFound {
		t.Fatalf("reveal at reveal start (no commit yet): got %v", err)
	}
	// 恰好到达揭示截止：终态化，揭示不再接受。
	clock.advance(10)
	if err := r.SubmitReveal(RevealRecord{Sender: "a", Nonce: 1, Salt: []byte("s"), Payload: []byte("p")}); err != ErrRoundClosed {
		t.Fatalf("reveal exactly at reveal deadline: got %v", err)
	}
	if r.Status() != StatusFinalized {
		t.Fatalf("status = %s, want finalized", r.Status())
	}
}

// 创建时截止时间已过的轮次：提交立即被拒，轮次可被观察为终态。
func TestCreateRoundAlreadyExpired(t *testing.T) {
	clock := &fakeClock{t: time.Unix(11000, 0)}
	svc := newTestService(t, clock)
	past := clock.t.Add(-time.Hour)
	cfg := RoundConfig{ID: "old", CommitDeadline: past, RevealDeadline: past.Add(time.Minute), TimeoutPolicy: PolicyExcludeUnrevealed}
	r, err := svc.CreateRound(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SubmitCommit(CommitRecord{Sender: "a", Nonce: 1, Commitment: MakeCommitment(nil, []byte("p"))}); err != ErrRoundClosed {
		t.Fatalf("commit to expired round: got %v", err)
	}
	if r.Status() != StatusFinalized {
		t.Fatalf("status = %s, want finalized", r.Status())
	}
}
