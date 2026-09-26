package fairorder

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Round 为单轮提交-揭示排序的内存状态。
//
// 状态机：committing -> revealing -> finalized | aborted
// 转换仅由挂钟时间与配置的超时策略驱动，不依赖任何外部协调。
type Round struct {
	cfg   RoundConfig
	now   func() time.Time
	store Store

	mu      sync.Mutex
	status  RoundStatus
	commits map[string]CommitRecord
	reveals map[string]RevealRecord
	result  *OrderResult
	doneCh  chan struct{}
}

func commitKey(sender string, nonce uint64) string {
	return fmt.Sprintf("%s/%d", sender, nonce)
}

func newRound(cfg RoundConfig, now func() time.Time, store Store) *Round {
	return &Round{
		cfg:     cfg,
		now:     now,
		store:   store,
		status:  StatusCommitting,
		commits: make(map[string]CommitRecord),
		reveals: make(map[string]RevealRecord),
		doneCh:  make(chan struct{}),
	}
}

// advanceLocked 依据当前时间推进状态（不做终局处理），调用方持锁。
func (r *Round) advanceLocked(now time.Time) {
	if r.status == StatusFinalized || r.status == StatusAborted {
		return
	}
	if now.Before(r.cfg.CommitDeadline) {
		r.status = StatusCommitting
	} else if now.Before(r.cfg.RevealDeadline) {
		r.status = StatusRevealing
	}
}

// finalizeLocked 在揭示截止后按超时策略产出终态，调用方持锁。
func (r *Round) finalizeLocked(now time.Time) {
	r.advanceLocked(now)
	if r.status == StatusFinalized || r.status == StatusAborted {
		return
	}
	if now.Before(r.cfg.RevealDeadline) {
		return
	}

	unrevealed := len(r.commits) > len(r.reveals)
	if unrevealed && r.cfg.TimeoutPolicy == PolicyAbort {
		r.status = StatusAborted
		_ = r.store.AppendResult(r.cfg.ID, nil, true)
		close(r.doneCh)
		return
	}

	commitList := make([]CommitRecord, 0, len(r.commits))
	for _, c := range r.commits {
		commitList = append(commitList, c)
	}
	revealList := make([]RevealRecord, 0, len(r.reveals))
	for _, rv := range r.reveals {
		revealList = append(revealList, rv)
	}
	r.result = buildOrder(r.cfg, commitList, revealList, now)
	r.status = StatusFinalized
	_ = r.store.AppendResult(r.cfg.ID, r.result, false)
	close(r.doneCh)
}

// Info 返回轮次只读快照。
func (r *Round) Info() RoundInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(r.now())
	return RoundInfo{
		Config:        r.cfg,
		Status:        r.status,
		CommitCount:   len(r.commits),
		RevealedCount: len(r.reveals),
	}
}

// Status 返回当前状态（必要时推进到终态）。
func (r *Round) Status() RoundStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(r.now())
	return r.status
}

// SubmitCommit 在提交阶段接受一笔不可逆承诺。
func (r *Round) SubmitCommit(rec CommitRecord) error {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(now)
	if r.status == StatusFinalized || r.status == StatusAborted {
		return ErrRoundClosed
	}
	if r.status != StatusCommitting {
		return ErrCommitPhaseClosed
	}
	k := commitKey(rec.Sender, rec.Nonce)
	if _, ok := r.commits[k]; ok {
		return ErrDuplicateCommit
	}
	if len(rec.Commitment) == 0 {
		return ErrInvalidCommitment
	}
	rec.Round = r.cfg.ID
	if rec.At.IsZero() {
		rec.At = now
	}
	// 先持久化（fsync）再对外确认，保证崩溃后提交不丢、不重。
	if err := r.store.AppendCommit(rec); err != nil {
		return err
	}
	r.commits[k] = rec
	return nil
}

// SubmitReveal 在揭示阶段校验并接受一笔有效揭示。
func (r *Round) SubmitReveal(rec RevealRecord) error {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(now)
	if r.status == StatusFinalized || r.status == StatusAborted {
		return ErrRoundClosed
	}
	if r.status != StatusRevealing {
		return ErrRevealNotOpen
	}
	k := commitKey(rec.Sender, rec.Nonce)
	commit, ok := r.commits[k]
	if !ok {
		return ErrCommitNotFound
	}
	if _, dup := r.reveals[k]; dup {
		return ErrDuplicateReveal
	}
	if !VerifyCommitment(commit.Commitment, rec.Salt, rec.Payload) {
		// 不匹配的揭示被拒绝，且不计为有效揭示。
		return ErrInvalidCommitment
	}
	rec.Round = r.cfg.ID
	if rec.At.IsZero() {
		rec.At = now
	}
	if err := r.store.AppendReveal(rec); err != nil {
		return err
	}
	r.reveals[k] = rec
	return nil
}

// Result 在轮次终态后返回已确认结果。
func (r *Round) Result() (*OrderResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(r.now())
	switch r.status {
	case StatusFinalized:
		return r.result, nil
	case StatusAborted:
		return nil, ErrRoundAborted
	default:
		return nil, ErrRoundNotFinalized
	}
}

// WaitFinalized 阻塞直到轮次到达终态（由后台 tick 或外部时钟驱动）或 ctx 取消。
func (r *Round) WaitFinalized(ctx context.Context) (RoundStatus, error) {
	ch := r.doneCh
	if s := r.Status(); s == StatusFinalized || s == StatusAborted {
		return s, nil
	}
	select {
	case <-ch:
		return r.Status(), nil
	case <-ctx.Done():
		return r.Status(), ctx.Err()
	}
}

// tick 由服务后台定时器调用，按注入时钟推进轮次。
func (r *Round) tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeLocked(now)
}

// restoreRound 从持久层快照重建一轮的内存状态。
func restoreRound(cfg RoundConfig, now func() time.Time, store Store, snap *snapshot) *Round {
	r := newRound(cfg, now, store)
	for k, c := range snap.commits[cfg.ID] {
		r.commits[k] = c
	}
	for k, rv := range snap.reveals[cfg.ID] {
		r.reveals[k] = rv
	}
	if res, ok := snap.results[cfg.ID]; ok && res != nil {
		r.result = res
		r.status = StatusFinalized
	} else if snap.aborted[cfg.ID] {
		r.status = StatusAborted
	} else {
		r.advanceLocked(now())
	}
	return r
}
