package fairorder

import (
	"context"
	"sync"
	"time"
)

// Service 管理多个相互隔离的排序轮次。不同轮次的提交、揭示、结果互不可见。
type Service struct {
	cfg   ServiceConfig
	store Store
	now   func() time.Time

	mu     sync.Mutex
	rounds map[RoundID]*Round
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewService 创建排序服务；当 DataDir 非空时打开磁盘日志并从中恢复，
// 为空时使用内存模式（不提供崩溃恢复）。
func NewService(cfg ServiceConfig) (*Service, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.TickInterval
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}

	var st Store
	var err error
	if cfg.DataDir != "" {
		st, err = newFileStore(cfg.DataDir)
	} else {
		st = newMemStore()
	}
	if err != nil {
		return nil, err
	}

	snap, err := st.Load()
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	s := &Service{
		cfg:    cfg,
		store:  st,
		now:    now,
		rounds: make(map[RoundID]*Round),
		stopCh: make(chan struct{}),
	}
	for id, rc := range snap.rounds {
		s.rounds[id] = restoreRound(rc, now, st, snap)
	}

	s.wg.Add(1)
	go s.loop(interval)
	return s, nil
}

// CreateRound 创建并持久化一个新轮次。
func (s *Service) CreateRound(rc RoundConfig) (*Round, error) {
	if rc.ID == "" {
		return nil, ErrInvalidConfig
	}
	if rc.RevealDeadline.IsZero() || !rc.RevealDeadline.After(rc.CommitDeadline) {
		return nil, ErrInvalidConfig
	}
	if rc.CommitDeadline.IsZero() {
		return nil, ErrInvalidConfig
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rounds[rc.ID]; ok {
		return nil, ErrRoundExists
	}
	if err := s.store.AppendRound(rc); err != nil {
		return nil, err
	}
	r := newRound(rc, s.now, s.store)
	s.rounds[rc.ID] = r
	// 创建时立即按当前时间推进一次，保证截止时间已过的轮次也能被正确观察。
	r.tick(s.now())
	return r, nil
}

// GetRound 返回指定轮次；不同轮次之间相互隔离。
func (s *Service) GetRound(id RoundID) (*Round, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rounds[id]
	if !ok {
		return nil, ErrRoundNotFound
	}
	return r, nil
}

// ListRounds 返回当前服务管理的全部轮次 ID（顺序确定）。
func (s *Service) ListRounds() []RoundID {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]RoundID, 0, len(s.rounds))
	for id := range s.rounds {
		ids = append(ids, id)
	}
	// 简单字典序，保证可复现。
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	return ids
}

// WaitRoundFinalized 阻塞直到指定轮次到达终态。
func (s *Service) WaitRoundFinalized(ctx context.Context, id RoundID) (RoundStatus, error) {
	r, err := s.GetRound(id)
	if err != nil {
		return 0, err
	}
	return r.WaitFinalized(ctx)
}

// tick 对所有轮次推进一次，供测试用注入时钟时手动驱动。
func (s *Service) tick(now time.Time) {
	s.mu.Lock()
	rounds := make([]*Round, 0, len(s.rounds))
	for _, r := range s.rounds {
		rounds = append(rounds, r)
	}
	s.mu.Unlock()
	for _, r := range rounds {
		r.tick(now)
	}
}

func (s *Service) loop(interval time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

// Close 停止后台调度并关闭持久层。已确认结果不受影响。
func (s *Service) Close() error {
	s.mu.Lock()
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.mu.Unlock()
	s.wg.Wait()
	return s.store.Close()
}
