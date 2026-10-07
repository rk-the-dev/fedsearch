// Package audit keeps an append-only JSONL record of every query: who asked,
// through which surface, what it touched, what it cost and how it ended.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record struct {
	At        time.Time `json:"at"`
	Principal string    `json:"principal"`
	Surface   string    `json:"surface"` // ui | api | cli | mcp | investigate
	Action    string    `json:"action"`  // plan | submit | complete | deny | nl | tool
	Tool      string    `json:"tool,omitempty"`
	QueryHash string    `json:"query_hash,omitempty"`
	JobID     string    `json:"job_id,omitempty"`
	Dataset   string    `json:"dataset,omitempty"`
	Slices    []string  `json:"slices,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	USD       float64   `json:"usd,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
}

type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{path: path, f: f}, nil
}

func (l *Log) Write(r Record) {
	if l == nil {
		return
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	b, _ := json.Marshal(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

// Recent returns up to n most recent records (newest first).
func (l *Log) Recent(n int) ([]Record, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			all = append(all, r)
		}
	}
	out := make([]Record, 0, n)
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, sc.Err()
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
