package fairorder

import "sync"

// snapshot 为从持久层重放出的轮次状态。
//
// 持久层是只追加日志，记录四类事件：轮次创建、提交、有效揭示、轮次结束
// （完成或终止）。重放这些事件即可无歧义地重建任意轮次：
//   - 同一 (round, sender, nonce) 的重复提交/揭示在重放时被折叠为一条；
//   - 已结束（ finalized / aborted ）的轮次直接恢复到终态；
//   - 未结束的轮次恢复到崩溃时所处阶段，可继续提交/揭示或被调度结束。
type snapshot struct {
	rounds  map[RoundID]RoundConfig
	commits map[RoundID]map[string]CommitRecord
	reveals map[RoundID]map[string]RevealRecord
	results map[RoundID]*OrderResult
	aborted map[RoundID]bool
}

func newSnapshot() *snapshot {
	return &snapshot{
		rounds:  make(map[RoundID]RoundConfig),
		commits: make(map[RoundID]map[string]CommitRecord),
		reveals: make(map[RoundID]map[string]RevealRecord),
		results: make(map[RoundID]*OrderResult),
		aborted: make(map[RoundID]bool),
	}
}

// Store 为轮次事件的只追加持久层。
type Store interface {
	AppendRound(rc RoundConfig) error
	AppendCommit(rec CommitRecord) error
	AppendReveal(rec RevealRecord) error
	// AppendResult 持久化终态：aborted=true 时 res 可为 nil。
	AppendResult(id RoundID, res *OrderResult, aborted bool) error
	Load() (*snapshot, error)
	Close() error
}

// memStore 为不提供崩溃恢复的内存持久层，供测试与无盘环境使用。
type memStore struct {
	mu   sync.Mutex
	snap *snapshot
}

func newMemStore() *memStore {
	return &memStore{snap: newSnapshot()}
}

func (m *memStore) AppendRound(rc RoundConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snap.rounds[rc.ID]; !ok {
		m.snap.rounds[rc.ID] = rc
	}
	return nil
}

func (m *memStore) AppendCommit(rec CommitRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket := m.snap.commits[rec.Round]
	if bucket == nil {
		bucket = make(map[string]CommitRecord)
		m.snap.commits[rec.Round] = bucket
	}
	k := commitKey(rec.Sender, rec.Nonce)
	if _, ok := bucket[k]; !ok {
		bucket[k] = rec
	}
	return nil
}

func (m *memStore) AppendReveal(rec RevealRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket := m.snap.reveals[rec.Round]
	if bucket == nil {
		bucket = make(map[string]RevealRecord)
		m.snap.reveals[rec.Round] = bucket
	}
	k := commitKey(rec.Sender, rec.Nonce)
	if _, ok := bucket[k]; !ok {
		bucket[k] = rec
	}
	return nil
}

func (m *memStore) AppendResult(id RoundID, res *OrderResult, aborted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if aborted {
		m.snap.aborted[id] = true
	} else {
		m.snap.results[id] = res
	}
	return nil
}

func (m *memStore) Load() (*snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 返回副本，避免调用方修改内部状态。
	out := newSnapshot()
	for id, rc := range m.snap.rounds {
		out.rounds[id] = rc
	}
	for id, bucket := range m.snap.commits {
		for k, v := range bucket {
			if out.commits[id] == nil {
				out.commits[id] = make(map[string]CommitRecord)
			}
			out.commits[id][k] = v
		}
	}
	for id, bucket := range m.snap.reveals {
		for k, v := range bucket {
			if out.reveals[id] == nil {
				out.reveals[id] = make(map[string]RevealRecord)
			}
			out.reveals[id][k] = v
		}
	}
	for id, r := range m.snap.results {
		out.results[id] = r
	}
	for id, b := range m.snap.aborted {
		out.aborted[id] = b
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }
