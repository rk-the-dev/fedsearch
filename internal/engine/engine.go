// Package engine defines the contract every query backend implements. It is
// asynchronous (submit, poll, fetch, cancel) because real backends are: Athena
// and SIEM search APIs run jobs. A synchronous engine such as embedded DuckDB
// is wrapped by the same runner, so the executor treats local and remote
// engines identically.
package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/result"
)

type Handle string

type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Done      State = "done"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

type Status struct {
	State State  `json:"state"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
	Err   string `json:"error,omitempty"`
}

// Page is one batch of results. Bytes is the cumulative bytes scanned.
type Page struct {
	Rows   []result.Row
	Groups []*result.Group
	Capped bool // aggregate: the slice hit its group cap
	Bytes  int64
	Done   bool
}

type Engine interface {
	Kind() string
	Estimate(ctx context.Context, s *planner.Slice, q *ir.Query) (planner.Estimate, error)
	Submit(ctx context.Context, s *planner.Slice, q *ir.Query) (Handle, error)
	Poll(ctx context.Context, h Handle) (Status, error)
	Fetch(ctx context.Context, h Handle) (Page, error)
	Cancel(ctx context.Context, h Handle) error
}

// Adapter is what a concrete backend implements; Runner supplies the async
// mechanics on top of it.
type Adapter interface {
	Kind() string
	Estimate(ctx context.Context, s *planner.Slice, q *ir.Query) (planner.Estimate, error)
	// Produce runs the slice and emits pages until done or ctx is cancelled.
	Produce(ctx context.Context, s *planner.Slice, q *ir.Query, emit func(Page) error) error
}

// Runner adapts a producer-style Adapter to the async Engine contract.
type Runner struct {
	Adapter
	seq  atomic.Int64
	mu   sync.Mutex
	jobs map[Handle]*run
}

type run struct {
	ch     chan Page
	cancel context.CancelFunc
	mu     sync.Mutex
	st     Status
}

func NewRunner(a Adapter) *Runner { return &Runner{Adapter: a, jobs: map[Handle]*run{}} }

func (r *Runner) Submit(ctx context.Context, s *planner.Slice, q *ir.Query) (Handle, error) {
	h := Handle(fmt.Sprintf("%s-%d", r.Kind(), r.seq.Add(1)))
	jctx, cancel := context.WithCancel(ctx)
	j := &run{ch: make(chan Page, 4), cancel: cancel, st: Status{State: Running}}
	r.mu.Lock()
	r.jobs[h] = j
	r.mu.Unlock()
	go func() {
		defer close(j.ch)
		err := r.Produce(jctx, s, q, func(p Page) error {
			j.mu.Lock()
			j.st.Rows += int64(len(p.Rows) + len(p.Groups))
			if p.Bytes > j.st.Bytes {
				j.st.Bytes = p.Bytes
			}
			j.mu.Unlock()
			select {
			case j.ch <- p:
				return nil
			case <-jctx.Done():
				return jctx.Err()
			}
		})
		j.mu.Lock()
		switch {
		case jctx.Err() != nil:
			j.st.State, j.st.Err = Cancelled, jctx.Err().Error()
		case err != nil:
			j.st.State, j.st.Err = Failed, err.Error()
		default:
			j.st.State = Done
		}
		j.mu.Unlock()
	}()
	return h, nil
}

func (r *Runner) get(h Handle) (*run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[h]
	if j == nil {
		return nil, fmt.Errorf("unknown handle %s", h)
	}
	return j, nil
}

func (r *Runner) Poll(_ context.Context, h Handle) (Status, error) {
	j, err := r.get(h)
	if err != nil {
		return Status{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st, nil
}

// Fetch blocks for the next page; Done is set when the producer finished.
func (r *Runner) Fetch(ctx context.Context, h Handle) (Page, error) {
	j, err := r.get(h)
	if err != nil {
		return Page{}, err
	}
	select {
	case p, ok := <-j.ch:
		if !ok {
			st, _ := r.Poll(ctx, h)
			r.mu.Lock()
			delete(r.jobs, h)
			r.mu.Unlock()
			if st.State == Failed || st.State == Cancelled {
				return Page{Done: true, Bytes: st.Bytes}, fmt.Errorf("%s", st.Err)
			}
			return Page{Done: true, Bytes: st.Bytes}, nil
		}
		return p, nil
	case <-ctx.Done():
		return Page{}, ctx.Err()
	}
}

func (r *Runner) Cancel(_ context.Context, h Handle) error {
	j, err := r.get(h)
	if err != nil {
		return nil // already finished
	}
	j.cancel()
	return nil
}
