package fairorder

import (
	"bytes"
	"sort"
	"time"
)

// round holds the mutable state of one ordering round. It is not safe for
// concurrent use; the Service serializes access.
type round struct {
	id             RoundID
	commitDeadline time.Time
	revealDeadline time.Time

	commits map[Commitment]struct{}
	reveals map[Commitment]RevealedTx

	finalized *RoundResult
	discarded bool
}

// newRound creates a round starting at start.
func newRound(id RoundID, start time.Time, cfg Config) *round {
	return &round{
		id:             id,
		commitDeadline: start.Add(cfg.CommitWindow),
		revealDeadline: start.Add(cfg.CommitWindow + cfg.RevealWindow),
		commits:        make(map[Commitment]struct{}),
		reveals:        make(map[Commitment]RevealedTx),
	}
}

// phaseAt reports the phase of the round at time t. A round past its reveal
// deadline but not yet finalized reports PhaseReveal (awaiting Finalize).
func (r *round) phaseAt(t time.Time) Phase {
	switch {
	case r.discarded:
		return PhaseDiscarded
	case r.finalized != nil:
		return PhaseFinalized
	case t.Before(r.commitDeadline):
		return PhaseCommit
	default:
		return PhaseReveal
	}
}

// canCommit validates a commitment without mutating state.
func (r *round) canCommit(c Commitment, maxCommits int) error {
	if _, ok := r.commits[c]; ok {
		return ErrDuplicateCommit
	}
	if maxCommits > 0 && len(r.commits) >= maxCommits {
		return ErrTooManyCommits
	}
	return nil
}

// applyCommit records a commitment. canCommit must have passed.
func (r *round) applyCommit(c Commitment) {
	r.commits[c] = struct{}{}
}

// canReveal validates a reveal without mutating state.
func (r *round) canReveal(c Commitment, payload []byte, salt Salt) error {
	if _, ok := r.commits[c]; !ok {
		return ErrUnknownCommitment
	}
	if _, ok := r.reveals[c]; ok {
		return ErrDuplicateReveal
	}
	if !VerifyCommitment(r.id, c, payload, salt) {
		return ErrCommitmentMismatch
	}
	return nil
}

// applyReveal records a reveal. canReveal must have passed.
func (r *round) applyReveal(c Commitment, payload []byte, salt Salt) {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	r.reveals[c] = RevealedTx{Commitment: c, Payload: cp, Salt: salt}
}

// computeResult derives the deterministic result without mutating state.
// Unrevealed commits are excluded; with UnrevealedFlag they are also
// reported, sorted by the same public OrderKey so the report itself is
// deterministic.
func (r *round) computeResult(t time.Time, policy UnrevealedPolicy) RoundResult {
	txs := make([]RevealedTx, 0, len(r.reveals))
	for _, tx := range r.reveals {
		txs = append(txs, tx)
	}
	ordered := SortRevealed(r.id, txs)

	var timedOut []Commitment
	if policy == UnrevealedFlag {
		for c := range r.commits {
			if _, ok := r.reveals[c]; !ok {
				timedOut = append(timedOut, c)
			}
		}
		sort.Slice(timedOut, func(i, j int) bool {
			ki, kj := OrderKey(r.id, timedOut[i]), OrderKey(r.id, timedOut[j])
			if cmp := bytes.Compare(ki[:], kj[:]); cmp != 0 {
				return cmp < 0
			}
			return bytes.Compare(timedOut[i][:], timedOut[j][:]) < 0
		})
	}

	return RoundResult{
		RoundID:     r.id,
		Ordered:     ordered,
		TimedOut:    timedOut,
		FinalizedAt: t,
	}
}
