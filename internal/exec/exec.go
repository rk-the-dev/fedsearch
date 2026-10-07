// Package exec runs plans as jobs: one goroutine per slice under per-engine
// concurrency limits, per-slice deadlines, cancellation that reaches the
// engines, budget enforcement on actual bytes, and an event stream that lets
// clients watch fast slices finish before slow ones.
package exec

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/cost"
	"github.com/rksurwase/fedsearch/internal/engine"
	"github.com/rksurwase/fedsearch/internal/enrich"
	"github.com/rksurwase/fedsearch/internal/guard"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/merge"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/result"
	"github.com/rksurwase/fedsearch/internal/schema"
)

type State string

const (
	Planned              State = "planned"
	AwaitingConfirmation State = "awaiting_confirmation"
	Running              State = "running"
	Completed            State = "completed"
	Partial              State = "partial"
	Cancelled            State = "cancelled"
	Failed               State = "failed"
)

func (s State) Terminal() bool {
	return s == Completed || s == Partial || s == Cancelled || s == Failed
}

type SliceStatus struct {
	ID        string        `json:"id"`
	Location  string        `json:"location"`
	Tier      string        `json:"tier"`
	State     string        `json:"state"` // queued | running | done | failed | timeout | cancelled
	Rows      int64         `json:"rows"`
	Bytes     int64         `json:"bytes"`
	USD       float64       `json:"usd"`
	Latency   time.Duration `json:"latency_ns"`
	FirstPage time.Duration `json:"first_page_ns,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type Result struct {
	Rows      []result.Row        `json:"rows,omitempty"`
	Groups    []result.FinalGroup `json:"groups,omitempty"`
	Merge     merge.RowStats      `json:"merge"`
	Bytes     int64               `json:"bytes_scanned"`
	USD       float64             `json:"usd"`
	Elapsed   time.Duration       `json:"elapsed_ns"`
	Slices    []*SliceStatus      `json:"slices"`
	Warnings  []string            `json:"warnings,omitempty"`
	Flagged   int                 `json:"flagged_values"`
	Approx    bool                `json:"approx,omitempty"`
	GroupsCap bool                `json:"may_be_inexact,omitempty"`
	Enriched  []string            `json:"enriched_fields,omitempty"`
	Status    State               `json:"status"`
	Reason    string              `json:"reason,omitempty"`
	ExtraInfo map[string]any      `json:"extra,omitempty"`
	byID      map[string]*SliceStatus
}

type Event struct {
	Seq  int       `json:"seq"`
	Type string    `json:"type"` // plan | state | slice | rows | stats | result | done
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

type Job struct {
	ID        string        `json:"id"`
	Principal string        `json:"principal"`
	State     State         `json:"state"`
	Plan      *planner.Plan `json:"plan"`
	Decision  cost.Decision `json:"decision"`
	CreatedAt time.Time     `json:"created_at"`
	Finished  time.Time     `json:"finished_at,omitempty"`
	Result    *Result       `json:"result,omitempty"`

	mu      sync.Mutex
	events  []Event
	subs    []chan Event
	cancel  context.CancelFunc
	confirm chan bool
	done    chan struct{}
	reason  string
}

// Snapshot returns a copy safe to serialize.
func (j *Job) Snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Job{ID: j.ID, Principal: j.Principal, State: j.State, Plan: j.Plan, Decision: j.Decision,
		CreatedAt: j.CreatedAt, Finished: j.Finished, Result: j.Result}
}

func (j *Job) emit(typ string, data any) {
	j.mu.Lock()
	ev := Event{Seq: len(j.events) + 1, Type: typ, At: time.Now().UTC(), Data: data}
	j.events = append(j.events, ev)
	subs := append([]chan Event(nil), j.subs...)
	j.mu.Unlock()
	for _, s := range subs {
		select {
		case s <- ev:
		default: // slow subscriber: it can replay from Events()
		}
	}
}

func (j *Job) setState(s State) {
	j.mu.Lock()
	j.State = s
	j.mu.Unlock()
	j.emit("state", map[string]any{"state": s, "reason": j.reason})
}

// Subscribe returns past events and a channel of future ones.
func (j *Job) Subscribe() ([]Event, <-chan Event, func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	ch := make(chan Event, 256)
	j.subs = append(j.subs, ch)
	past := append([]Event(nil), j.events...)
	return past, ch, func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		for i, s := range j.subs {
			if s == ch {
				j.subs = append(j.subs[:i], j.subs[i+1:]...)
				break
			}
		}
	}
}

// Done is closed when the job reaches a terminal state.
func (j *Job) Done() <-chan struct{} { return j.done }

// Manager owns jobs and the shared execution resources.
type Manager struct {
	Engines  map[string]engine.Engine // by source kind
	Enricher *enrich.Enricher
	Ledger   *cost.Ledger
	Budget   config.Budget
	sems     map[string]chan struct{} // per source
	mu       sync.Mutex
	jobs     map[string]*Job
	seq      int
	OnFinish func(*Job)
	Lookup   schema.Lookup
}

func NewManager(cfg *config.Config, engines map[string]engine.Engine, en *enrich.Enricher, l *cost.Ledger, lookup schema.Lookup) *Manager {
	m := &Manager{Engines: engines, Enricher: en, Ledger: l, Budget: cfg.Budget, sems: map[string]chan struct{}{}, jobs: map[string]*Job{}, Lookup: lookup}
	for _, s := range cfg.Sources {
		n := s.MaxConc
		if n <= 0 {
			n = 2
		}
		m.sems[s.Name] = make(chan struct{}, n)
	}
	return m
}

func (m *Manager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// Submit creates a job. Jobs above the confirmation threshold wait for
// Confirm unless autoConfirm; denied plans are rejected outright.
func (m *Manager) Submit(p *planner.Plan, principal string, d cost.Decision, autoConfirm bool) (*Job, error) {
	if !d.Allowed {
		return nil, fmt.Errorf("budget: %s", d.Reason)
	}
	m.mu.Lock()
	m.seq++
	j := &Job{ID: fmt.Sprintf("j%04d-%s", m.seq, p.ID[:6]), Principal: principal, State: Planned, Plan: p, Decision: d,
		CreatedAt: time.Now().UTC(), confirm: make(chan bool, 1), done: make(chan struct{})}
	m.jobs[j.ID] = j
	m.mu.Unlock()
	j.emit("plan", p)
	if d.NeedsConfirm && !autoConfirm {
		j.reason = d.Reason
		j.setState(AwaitingConfirmation)
		go func() {
			select {
			case ok := <-j.confirm:
				if ok {
					m.start(j)
					return
				}
				j.reason = "rejected by user"
			case <-time.After(10 * time.Minute):
				j.reason = "confirmation timed out"
			}
			m.finish(j, &Result{Status: Cancelled, Reason: j.reason})
		}()
		return j, nil
	}
	m.start(j)
	return j, nil
}

func (m *Manager) Confirm(id string, ok bool) error {
	j := m.Get(id)
	if j == nil {
		return fmt.Errorf("no job %s", id)
	}
	if j.Snapshot().State != AwaitingConfirmation {
		return fmt.Errorf("job %s is %s", id, j.Snapshot().State)
	}
	j.confirm <- ok
	return nil
}

func (m *Manager) Cancel(id string) error {
	j := m.Get(id)
	if j == nil {
		return fmt.Errorf("no job %s", id)
	}
	switch st := j.Snapshot().State; {
	case st == AwaitingConfirmation:
		j.confirm <- false
	case st.Terminal():
	default:
		j.mu.Lock()
		j.reason = "cancelled by user"
		cancel := j.cancel
		j.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return nil
}

func (m *Manager) start(j *Job) {
	var deadline time.Duration
	for _, s := range j.Plan.Slices {
		deadline = max(deadline, s.Timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline+5*time.Second)
	j.mu.Lock()
	j.cancel = cancel
	j.mu.Unlock()
	j.setState(Running)
	go m.run(ctx, cancel, j)
}

type sliceOut struct {
	rows   []result.Row
	groups []*result.Group
	capped bool
	ok     bool
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, j *Job) {
	defer cancel()
	start := time.Now()
	q := j.Plan.Query
	res := &Result{byID: map[string]*SliceStatus{}, Warnings: j.Plan.Warnings}
	var mu sync.Mutex
	outs := make([]sliceOut, len(j.Plan.Slices))
	var spent float64

	var wg sync.WaitGroup
	for i, s := range j.Plan.Slices {
		st := &SliceStatus{ID: s.ID, Location: s.Location, Tier: s.Tier, State: "queued"}
		res.Slices = append(res.Slices, st)
		res.byID[s.ID] = st
		wg.Add(1)
		go func(i int, s *planner.Slice, st *SliceStatus) {
			defer wg.Done()
			ok := m.runSlice(ctx, j, s, q, st, &outs[i], &mu, func(deltaUSD float64) {
				mu.Lock()
				spent += deltaUSD
				over := spent > m.Budget.MaxPerQueryUSD
				mu.Unlock()
				if over {
					j.mu.Lock()
					j.reason = fmt.Sprintf("actual cost $%.4f exceeded the per-query cap $%.2f", spent, m.Budget.MaxPerQueryUSD)
					j.mu.Unlock()
					cancel()
				}
			})
			outs[i].ok = ok
		}(i, s, st)
	}
	wg.Wait()

	okCount := 0
	for _, o := range outs {
		if o.ok {
			okCount++
		}
	}
	for _, st := range res.Slices {
		res.Bytes += st.Bytes
		res.USD += st.USD
	}
	j.mu.Lock()
	reason := j.reason
	j.mu.Unlock()
	switch {
	case reason != "" && okCount < len(outs):
		res.Status, res.Reason = Cancelled, reason
	case len(outs) == 0:
		res.Status, res.Reason = Completed, "no location covers the requested time range"
	case okCount == len(outs):
		res.Status = Completed
	case okCount == 0:
		res.Status, res.Reason = Failed, "all slices failed"
	default:
		res.Status, res.Reason = Partial, fmt.Sprintf("%d of %d slices did not complete; their time ranges are missing", len(outs)-okCount, len(outs))
	}

	if q.IsAggregate() {
		var parts [][]*result.Group
		for _, o := range outs {
			if o.ok {
				parts = append(parts, o.groups)
				res.GroupsCap = res.GroupsCap || o.capped
			}
		}
		res.Groups = result.Finalize(q, merge.Groups(parts))
		for _, g := range res.Groups {
			res.Approx = res.Approx || g.Approx
		}
	} else {
		var streams [][]result.Row
		for _, o := range outs {
			if o.ok {
				streams = append(streams, o.rows)
			}
		}
		desc := q.Order != nil && q.Order.Desc
		res.Rows, res.Merge = merge.Rows(streams, desc, q.Limit)
		res.Flagged = flagSuspicious(res.Rows, m.Lookup, q.Dataset)
		if len(q.Enrich) > 0 && m.Enricher != nil {
			if err := m.Enricher.Rows(ctx, res.Rows, q.Enrich); err != nil {
				res.Warnings = append(res.Warnings, "enrichment unavailable: "+err.Error())
			} else {
				res.Enriched = q.Enrich
			}
		}
	}
	res.Elapsed = time.Since(start)
	m.Ledger.Charge(j.Principal, res.USD)
	m.finish(j, res)
}

// runSlice executes one slice to completion and reports progress events.
func (m *Manager) runSlice(ctx context.Context, j *Job, s *planner.Slice, q *ir.Query, st *SliceStatus, out *sliceOut, mu *sync.Mutex, charge func(float64)) bool {
	eng := m.Engines[s.Kind]
	if eng == nil {
		st.State, st.Error = "failed", "no engine for "+s.Kind
		j.emit("slice", *st)
		return false
	}
	sem := m.sems[s.Source]
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		st.State, st.Error = "cancelled", ctx.Err().Error()
		j.emit("slice", *st)
		return false
	}
	sctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	t0 := time.Now()
	mu.Lock()
	st.State = "running"
	mu.Unlock()
	j.emit("slice", *st)

	h, err := eng.Submit(sctx, s, q)
	if err != nil {
		st.State, st.Error = "failed", err.Error()
		j.emit("slice", *st)
		return false
	}
	var lastUSD float64
	for {
		page, err := eng.Fetch(sctx, h)
		if err != nil {
			_ = eng.Cancel(context.Background(), h) // propagate to the engine
			mu.Lock()
			st.Latency = time.Since(t0)
			switch {
			case errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
				st.State, st.Error = "timeout", fmt.Sprintf("exceeded %s deadline", s.Timeout)
			case ctx.Err() != nil:
				st.State, st.Error = "cancelled", "job cancelled"
			default:
				st.State, st.Error = "failed", err.Error()
			}
			snap := *st
			mu.Unlock()
			j.emit("slice", snap)
			return false
		}
		mu.Lock()
		if st.FirstPage == 0 {
			st.FirstPage = time.Since(t0)
		}
		if page.Bytes > st.Bytes {
			st.Bytes = page.Bytes
		}
		st.USD = cost.Price(s.Loc.Cost, st.Bytes)
		delta := st.USD - lastUSD
		lastUSD = st.USD
		for _, r := range page.Rows {
			if s.Residual != nil && !ir.Eval(s.Residual, r.Get) {
				continue
			}
			out.rows = append(out.rows, r)
			st.Rows++
		}
		out.groups = append(out.groups, page.Groups...)
		out.capped = out.capped || page.Capped
		st.Rows += int64(len(page.Groups))
		snap := *st
		mu.Unlock()
		charge(delta)
		if len(page.Rows) > 0 {
			preview := page.Rows
			if len(preview) > 200 {
				preview = preview[:200]
			}
			j.emit("rows", map[string]any{"slice": s.ID, "rows": preview, "total": snap.Rows})
		}
		if page.Done {
			mu.Lock()
			st.State, st.Latency = "done", time.Since(t0)
			snap = *st
			mu.Unlock()
			j.emit("slice", snap)
			return true
		}
		j.emit("slice", snap)
	}
}

func (m *Manager) finish(j *Job, res *Result) {
	j.mu.Lock()
	j.Result = res
	j.State = res.Status
	j.Finished = time.Now().UTC()
	j.mu.Unlock()
	j.emit("result", res)
	j.emit("done", map[string]any{"state": res.Status, "reason": res.Reason})
	close(j.done)
	if m.OnFinish != nil {
		m.OnFinish(j)
	}
}

// flagSuspicious marks rows whose free-text fields look like prompt injection.
func flagSuspicious(rows []result.Row, l schema.Lookup, dataset string) int {
	n := 0
	for i := range rows {
		for path, v := range rows[i].Fields {
			if _, role, ok := l.Field(dataset, path); ok && role == schema.RoleFreeText {
				if s, ok := v.(string); ok && guard.Suspicious(s) {
					rows[i].Flags = append(rows[i].Flags, "suspicious:"+path)
					n++
				}
			}
		}
	}
	return n
}
