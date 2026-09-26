package fairorder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// 磁盘日志格式：数据目录下的 roundlog.jsonl，每行一条 JSON 事件。
//
// 每条事件在追加后立即 fsync，保证“已确认”的提交/揭示/结果在进程崩溃或
// 掉电后不丢失；恢复时按顺序重放即可重建状态。日志只追加、不修改，重复
// （崩溃发生在写入与应答之间）在重放时按 (round, sender, nonce) 幂等折叠。

const logFileName = "roundlog.jsonl"

type logEntry struct {
	Type    string        `json:"type"` // round | commit | reveal | result
	RoundID RoundID       `json:"round_id,omitempty"`
	Round   *RoundConfig  `json:"round_cfg,omitempty"`
	Commit  *CommitRecord `json:"commit,omitempty"`
	Reveal  *RevealRecord `json:"reveal,omitempty"`
	Result  *OrderResult  `json:"result,omitempty"`
	Aborted bool          `json:"aborted,omitempty"`
}

// FileStore 为基于 JSONL 只追加日志 + fsync 的持久层。
type FileStore struct {
	mu  sync.Mutex
	dir string
	f   *os.File
	w   *bufio.Writer
}

func newFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("fairorder: empty data dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("fairorder: create data dir: %w", err)
	}
	path := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fairorder: open log: %w", err)
	}
	fs := &FileStore{dir: dir, f: f, w: bufio.NewWriter(f)}
	return fs, nil
}

func (fs *FileStore) append(e *logEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("fairorder: encode log entry: %w", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, err := fs.w.Write(data); err != nil {
		return err
	}
	if err := fs.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := fs.w.Flush(); err != nil {
		return err
	}
	return fs.f.Sync() // fsync：崩溃后已确认事件不丢失
}

func (fs *FileStore) AppendRound(rc RoundConfig) error {
	return fs.append(&logEntry{Type: "round", Round: &rc})
}

func (fs *FileStore) AppendCommit(rec CommitRecord) error {
	return fs.append(&logEntry{Type: "commit", Commit: &rec})
}

func (fs *FileStore) AppendReveal(rec RevealRecord) error {
	return fs.append(&logEntry{Type: "reveal", Reveal: &rec})
}

func (fs *FileStore) AppendResult(id RoundID, res *OrderResult, aborted bool) error {
	return fs.append(&logEntry{Type: "result", RoundID: id, Result: res, Aborted: aborted})
}

func (fs *FileStore) Load() (*snapshot, error) {
	fs.mu.Lock()
	path := filepath.Join(fs.dir, logFileName)
	fs.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newSnapshot(), nil
		}
		return nil, fmt.Errorf("fairorder: open log for replay: %w", err)
	}
	defer f.Close()

	snap := newSnapshot()
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var e logEntry
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("fairorder: corrupt log: %w", err)
		}
		switch e.Type {
		case "round":
			if e.Round == nil {
				return nil, errors.New("fairorder: corrupt round entry")
			}
			if _, ok := snap.rounds[e.Round.ID]; !ok {
				snap.rounds[e.Round.ID] = *e.Round
			}
		case "commit":
			if e.Commit == nil {
				return nil, errors.New("fairorder: corrupt commit entry")
			}
			c := *e.Commit
			if snap.commits[c.Round] == nil {
				snap.commits[c.Round] = make(map[string]CommitRecord)
			}
			k := commitKey(c.Sender, c.Nonce)
			if _, ok := snap.commits[c.Round][k]; !ok {
				snap.commits[c.Round][k] = c
			}
		case "reveal":
			if e.Reveal == nil {
				return nil, errors.New("fairorder: corrupt reveal entry")
			}
			r := *e.Reveal
			if snap.reveals[r.Round] == nil {
				snap.reveals[r.Round] = make(map[string]RevealRecord)
			}
			k := commitKey(r.Sender, r.Nonce)
			if _, ok := snap.reveals[r.Round][k]; !ok {
				snap.reveals[r.Round][k] = r
			}
		case "result":
			if e.Aborted {
				// 终止事件只需要 round id；为兼容旧格式也接受 result 携带 id。
				id := e.RoundID
				if e.Result != nil {
					id = e.Result.Round
				}
				if id == "" {
					return nil, errors.New("fairorder: corrupt result entry")
				}
				snap.aborted[id] = true
				continue
			}
			if e.Result == nil {
				return nil, errors.New("fairorder: corrupt result entry")
			}
			snap.results[e.Result.Round] = e.Result
		default:
			return nil, fmt.Errorf("fairorder: unknown log entry type %q", e.Type)
		}
	}
	return snap, nil
}

func (fs *FileStore) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.w.Flush(); err != nil {
		_ = fs.f.Close()
		return err
	}
	return fs.f.Close()
}
