package fairorder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// EventType names the kind of a persisted WAL event.
type EventType string

const (
	EventRoundOpened EventType = "round_opened"
	EventCommitted   EventType = "committed"
	EventRevealed    EventType = "revealed"
	EventFinalized   EventType = "finalized"
	EventDiscarded   EventType = "discarded"
)

// Event is a single persisted state transition. Every mutation the Service
// confirms is appended to the Store (and fsynced for FileStore) before the
// operation returns, so a crash never loses confirmed results.
type Event struct {
	Type       EventType    `json:"type"`
	Round      RoundID      `json:"round"`
	Commitment *Commitment  `json:"commitment,omitempty"`
	Tx         *RevealedTx  `json:"tx,omitempty"`
	Result     *RoundResult `json:"result,omitempty"`
	// OpenedAt is the round start time in unix nanos (EventRoundOpened).
	OpenedAt int64 `json:"opened_at,omitempty"`
}

// Store persists and replays WAL events.
type Store interface {
	Append(e Event) error
	Load() ([]Event, error)
	Close() error
}

// MemStore is an in-memory Store, useful for tests.
type MemStore struct {
	mu     sync.Mutex
	events []Event
}

// Append records an event in memory.
func (m *MemStore) Append(e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

// Load returns all recorded events in order.
func (m *MemStore) Load() ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out, nil
}

// Close is a no-op for MemStore.
func (m *MemStore) Close() error { return nil }

// FileStore is a WAL backed by an append-only JSON-lines file. Each Append
// is followed by fsync before returning.
type FileStore struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// OpenFileStore opens (or creates) a WAL file at path.
func OpenFileStore(path string) (*FileStore, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fairorder: open store: %w", err)
	}
	return &FileStore{f: f, enc: json.NewEncoder(f)}, nil
}

// Append writes one event and fsyncs before returning.
func (fs *FileStore) Append(e Event) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.enc.Encode(e); err != nil {
		return fmt.Errorf("fairorder: encode event: %w", err)
	}
	if err := fs.f.Sync(); err != nil {
		return fmt.Errorf("fairorder: sync store: %w", err)
	}
	return nil
}

// Load replays all events from the file. A torn final line (crash during
// Append) is ignored: the fsync-before-ack rule guarantees such a line was
// never confirmed to any caller.
func (fs *FileStore) Load() ([]Event, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, err := fs.f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("fairorder: seek store: %w", err)
	}
	var events []Event
	sc := bufio.NewScanner(fs.f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			// Torn tail: stop replay here.
			break
		}
		events = append(events, e)
	}
	// Restore append position at end of file.
	if _, err := fs.f.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("fairorder: restore append position: %w", err)
	}
	return events, nil
}

// Close closes the underlying file.
func (fs *FileStore) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.f.Close()
}
