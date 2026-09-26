// Package fairorder implements a local fair transaction ordering service.
//
// The service collects transaction commitments, accepts reveals, and
// produces a deterministic, publicly verifiable ordering per round.
package fairorder

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// RoundID identifies a single ordering round.
type RoundID uint64

// Commitment is an irreversible commitment to a transaction payload.
type Commitment [32]byte

// String returns the hex encoding of the commitment.
func (c Commitment) String() string { return hex.EncodeToString(c[:]) }

// MarshalText encodes the commitment as hex.
func (c Commitment) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText decodes a hex commitment.
func (c *Commitment) UnmarshalText(b []byte) error {
	return unmarshalFixedHex(c[:], b)
}

// Salt is the random nonce revealed alongside the payload.
type Salt [32]byte

// String returns the hex encoding of the salt.
func (s Salt) String() string { return hex.EncodeToString(s[:]) }

// MarshalText encodes the salt as hex.
func (s Salt) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText decodes a hex salt.
func (s *Salt) UnmarshalText(b []byte) error {
	return unmarshalFixedHex(s[:], b)
}

func unmarshalFixedHex(dst, src []byte) error {
	n, err := hex.Decode(dst, src)
	if err != nil {
		return fmt.Errorf("fairorder: bad hex: %w", err)
	}
	if n != len(dst) {
		return fmt.Errorf("fairorder: want %d bytes, got %d", len(dst), n)
	}
	return nil
}

// Phase is the lifecycle stage of a round.
type Phase int

const (
	// PhaseCommit: commitments are accepted.
	PhaseCommit Phase = iota
	// PhaseReveal: reveals are accepted and verified.
	PhaseReveal
	// PhaseFinalized: ordering has been produced and persisted.
	PhaseFinalized
	// PhaseDiscarded: round was terminated by policy; nothing included.
	PhaseDiscarded
)

func (p Phase) String() string {
	switch p {
	case PhaseCommit:
		return "commit"
	case PhaseReveal:
		return "reveal"
	case PhaseFinalized:
		return "finalized"
	case PhaseDiscarded:
		return "discarded"
	default:
		return "unknown"
	}
}

// UnrevealedPolicy decides how commits without a valid reveal are handled
// once the reveal deadline passes.
type UnrevealedPolicy int

const (
	// UnrevealedExclude drops unrevealed commits silently.
	UnrevealedExclude UnrevealedPolicy = iota
	// UnrevealedFlag drops unrevealed commits but reports them in the result.
	UnrevealedFlag
)

// CrashPolicy decides how an unfinished round is treated on recovery.
type CrashPolicy int

const (
	// CrashResume keeps the round open with its persisted deadlines.
	CrashResume CrashPolicy = iota
	// CrashFinalize finalizes the round immediately with valid reveals.
	CrashFinalize
	// CrashDiscard terminates the round including no transactions.
	CrashDiscard
)

// Config controls round timing and policies.
type Config struct {
	CommitWindow     time.Duration
	RevealWindow     time.Duration
	UnrevealedPolicy UnrevealedPolicy
	CrashPolicy      CrashPolicy
	MaxCommits       int // 0 means unlimited
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	if c.CommitWindow <= 0 {
		return fmt.Errorf("%w: CommitWindow must be positive", ErrInvalidConfig)
	}
	if c.RevealWindow <= 0 {
		return fmt.Errorf("%w: RevealWindow must be positive", ErrInvalidConfig)
	}
	if c.UnrevealedPolicy != UnrevealedExclude && c.UnrevealedPolicy != UnrevealedFlag {
		return fmt.Errorf("%w: unknown UnrevealedPolicy %d", ErrInvalidConfig, c.UnrevealedPolicy)
	}
	if c.CrashPolicy != CrashResume && c.CrashPolicy != CrashFinalize && c.CrashPolicy != CrashDiscard {
		return fmt.Errorf("%w: unknown CrashPolicy %d", ErrInvalidConfig, c.CrashPolicy)
	}
	if c.MaxCommits < 0 {
		return fmt.Errorf("%w: MaxCommits must be >= 0", ErrInvalidConfig)
	}
	return nil
}

// RevealedTx is a transaction whose reveal has been verified.
type RevealedTx struct {
	Commitment Commitment
	Payload    []byte
	Salt       Salt
}

// RoundResult is the finalized, deterministic outcome of a round.
type RoundResult struct {
	RoundID     RoundID
	Ordered     []RevealedTx
	TimedOut    []Commitment
	FinalizedAt time.Time
}

// RoundInfo describes the current observable state of a round.
type RoundInfo struct {
	ID             RoundID
	Phase          Phase
	CommitDeadline time.Time
	RevealDeadline time.Time
	Commits        int
	Reveals        int
}

var (
	ErrNoActiveRound       = errors.New("fairorder: no active round")
	ErrRoundActive         = errors.New("fairorder: current round still active")
	ErrCommitPhaseOver     = errors.New("fairorder: commit phase has ended")
	ErrRevealNotStarted    = errors.New("fairorder: reveal phase has not started")
	ErrRevealPhaseOver     = errors.New("fairorder: reveal phase has ended")
	ErrDuplicateCommit     = errors.New("fairorder: duplicate commitment")
	ErrDuplicateReveal     = errors.New("fairorder: duplicate reveal")
	ErrUnknownCommitment   = errors.New("fairorder: unknown commitment")
	ErrCommitmentMismatch  = errors.New("fairorder: reveal does not match commitment")
	ErrRoundNotFinalized   = errors.New("fairorder: round not finalized yet")
	ErrTooManyCommits      = errors.New("fairorder: round commit limit reached")
	ErrEmptyPayload        = errors.New("fairorder: empty payload")
	ErrUnknownRound        = errors.New("fairorder: unknown round")
	ErrInvalidConfig       = errors.New("fairorder: invalid config")
	ErrFinalizeTooEarly    = errors.New("fairorder: reveal phase has not ended")
	ErrRoundDiscarded      = errors.New("fairorder: round was discarded")
	ErrServiceClosed       = errors.New("fairorder: service is closed")
	ErrResultRoundMismatch = errors.New("fairorder: result belongs to a different round")
	ErrResultDuplicate     = errors.New("fairorder: result contains a duplicate commitment")
	ErrResultBadReveal     = errors.New("fairorder: result contains an invalid reveal")
	ErrResultBadOrder      = errors.New("fairorder: result is not sorted by the public order key")
	ErrResultOverlap       = errors.New("fairorder: commitment both ordered and timed out")
)

// clone returns a deep copy of the result so callers cannot mutate
// service-internal state.
func (r RoundResult) clone() RoundResult {
	out := r
	if r.Ordered != nil {
		out.Ordered = make([]RevealedTx, len(r.Ordered))
		for i, tx := range r.Ordered {
			cp := make([]byte, len(tx.Payload))
			copy(cp, tx.Payload)
			out.Ordered[i] = RevealedTx{Commitment: tx.Commitment, Payload: cp, Salt: tx.Salt}
		}
	}
	if r.TimedOut != nil {
		out.TimedOut = make([]Commitment, len(r.TimedOut))
		copy(out.TimedOut, r.TimedOut)
	}
	return out
}
