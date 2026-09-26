package fairorder

import (
	"sync"
	"time"
)

// Service is the fair transaction ordering service. All methods are safe
// for concurrent use; the final ordering of a round is independent of the
// arrival order of commits and reveals.
type Service struct {
	mu        sync.Mutex
	cfg       Config
	store     Store
	clock     Clock
	cur       *round
	results   map[RoundID]RoundResult
	discarded map[RoundID]struct{}
	nextID    RoundID
	closed    bool
}

// NewService creates a Service, recovering state from the store. Rounds
// left unfinished by a crash are handled according to cfg.CrashPolicy.
// The Config must match the one used when the store was written: round
// deadlines are derived from the persisted start time and the configured
// windows.
func NewService(cfg Config, store Store, clock Clock) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || clock == nil {
		return nil, ErrInvalidConfig
	}
	s := &Service{
		cfg:       cfg,
		store:     store,
		clock:     clock,
		results:   make(map[RoundID]RoundResult),
		discarded: make(map[RoundID]struct{}),
		nextID:    1,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// recover replays the WAL and applies the crash policy.
func (s *Service) recover() error {
	events, err := s.store.Load()
	if err != nil {
		return err
	}
	rounds := make(map[RoundID]*round)
	for _, e := range events {
		switch e.Type {
		case EventRoundOpened:
			r := newRound(e.Round, time.Unix(0, e.OpenedAt), s.cfg)
			rounds[e.Round] = r
			s.cur = r
			if e.Round >= s.nextID {
				s.nextID = e.Round + 1
			}
		case EventCommitted:
			if r, ok := rounds[e.Round]; ok && e.Commitment != nil {
				r.applyCommit(*e.Commitment)
			}
		case EventRevealed:
			if r, ok := rounds[e.Round]; ok && e.Tx != nil {
				r.applyReveal(e.Tx.Commitment, e.Tx.Payload, e.Tx.Salt)
			}
		case EventFinalized:
			if r, ok := rounds[e.Round]; ok && e.Result != nil {
				res := *e.Result
				r.finalized = &res
				s.results[e.Round] = res
			}
			if s.cur != nil && s.cur.id == e.Round {
				s.cur = nil
			}
		case EventDiscarded:
			if r, ok := rounds[e.Round]; ok {
				r.discarded = true
			}
			s.discarded[e.Round] = struct{}{}
			if s.cur != nil && s.cur.id == e.Round {
				s.cur = nil
			}
		}
	}
	// A round left active at crash is settled per CrashPolicy.
	if s.cur != nil {
		switch s.cfg.CrashPolicy {
		case CrashResume:
			// Keep the round open with its persisted deadlines.
		case CrashFinalize:
			res := s.cur.computeResult(s.clock.Now(), s.cfg.UnrevealedPolicy)
			if err := s.store.Append(Event{Type: EventFinalized, Round: s.cur.id, Result: &res}); err != nil {
				return err
			}
			s.cur.finalized = &res
			s.results[s.cur.id] = res
			s.cur = nil
		case CrashDiscard:
			if err := s.store.Append(Event{Type: EventDiscarded, Round: s.cur.id}); err != nil {
				return err
			}
			s.cur.discarded = true
			s.discarded[s.cur.id] = struct{}{}
			s.cur = nil
		}
	}
	return nil
}

// OpenRound starts a new round if no round is currently active.
func (s *Service) OpenRound() (RoundID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrServiceClosed
	}
	if s.cur != nil {
		return 0, ErrRoundActive
	}
	id := s.nextID
	now := s.clock.Now()
	e := Event{Type: EventRoundOpened, Round: id, OpenedAt: now.UnixNano()}
	if err := s.store.Append(e); err != nil {
		return 0, err
	}
	s.nextID++
	s.cur = newRound(id, now, s.cfg)
	return id, nil
}

// Submit accepts a commitment into the current round's commit phase.
func (s *Service) Submit(c Commitment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	if s.cur == nil {
		return ErrNoActiveRound
	}
	if !s.clock.Now().Before(s.cur.commitDeadline) {
		return ErrCommitPhaseOver
	}
	if err := s.cur.canCommit(c, s.cfg.MaxCommits); err != nil {
		return err
	}
	if err := s.store.Append(Event{Type: EventCommitted, Round: s.cur.id, Commitment: &c}); err != nil {
		return err
	}
	s.cur.applyCommit(c)
	return nil
}

// Reveal verifies and records a reveal in the current round's reveal
// phase. The caller names the commitment being opened; the reveal is
// rejected unless payload+salt recompute to exactly that commitment in the
// current round.
func (s *Service) Reveal(c Commitment, payload []byte, salt Salt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	if s.cur == nil {
		return ErrNoActiveRound
	}
	if len(payload) == 0 {
		return ErrEmptyPayload
	}
	now := s.clock.Now()
	if now.Before(s.cur.commitDeadline) {
		return ErrRevealNotStarted
	}
	if !now.Before(s.cur.revealDeadline) {
		return ErrRevealPhaseOver
	}
	if err := s.cur.canReveal(c, payload, salt); err != nil {
		return err
	}
	tx := RevealedTx{Commitment: c, Payload: payload, Salt: salt}
	if err := s.store.Append(Event{Type: EventRevealed, Round: s.cur.id, Tx: &tx}); err != nil {
		return err
	}
	s.cur.applyReveal(c, payload, salt)
	return nil
}

// Finalize produces the deterministic ordering for the current round once
// its reveal deadline has passed, persists it, and closes the round.
func (s *Service) Finalize() (RoundResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RoundResult{}, ErrServiceClosed
	}
	if s.cur == nil {
		return RoundResult{}, ErrNoActiveRound
	}
	now := s.clock.Now()
	if now.Before(s.cur.revealDeadline) {
		return RoundResult{}, ErrFinalizeTooEarly
	}
	res := s.cur.computeResult(now, s.cfg.UnrevealedPolicy)
	if err := s.store.Append(Event{Type: EventFinalized, Round: s.cur.id, Result: &res}); err != nil {
		return RoundResult{}, err
	}
	s.cur.finalized = &res
	s.results[s.cur.id] = res
	s.cur = nil
	return res.clone(), nil
}

// CurrentRound describes the active round.
func (s *Service) CurrentRound() (RoundInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return RoundInfo{}, ErrNoActiveRound
	}
	r := s.cur
	return RoundInfo{
		ID:             r.id,
		Phase:          r.phaseAt(s.clock.Now()),
		CommitDeadline: r.commitDeadline,
		RevealDeadline: r.revealDeadline,
		Commits:        len(r.commits),
		Reveals:        len(r.reveals),
	}, nil
}

// Result returns the finalized result of a past round.
func (s *Service) Result(id RoundID) (RoundResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res, ok := s.results[id]; ok {
		return res.clone(), nil
	}
	if _, ok := s.discarded[id]; ok {
		return RoundResult{}, ErrRoundDiscarded
	}
	if s.cur != nil && s.cur.id == id {
		return RoundResult{}, ErrRoundNotFinalized
	}
	return RoundResult{}, ErrUnknownRound
}

// Close releases resources held by the service.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.store.Close()
}
