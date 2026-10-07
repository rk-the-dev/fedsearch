// Package service wires the system together behind one facade used by the
// HTTP API, the CLI, the MCP server and tests: catalog, planner, cost,
// engines, executor, enrichment, NL and audit.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/rksurwase/fedsearch/internal/audit"
	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/cost"
	"github.com/rksurwase/fedsearch/internal/engine"
	"github.com/rksurwase/fedsearch/internal/engine/duckdb"
	"github.com/rksurwase/fedsearch/internal/engine/opensearch"
	"github.com/rksurwase/fedsearch/internal/enrich"
	"github.com/rksurwase/fedsearch/internal/exec"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/schema"
)

type Service struct {
	Cfg      *config.Config
	Engines  map[string]engine.Engine
	Jobs     *exec.Manager
	Ledger   *cost.Ledger
	Enricher *enrich.Enricher
	Audit    *audit.Log
	Now      func() time.Time

	mu  sync.RWMutex
	cat *catalog.Catalog
}

func (s *Service) catalogPath() string { return filepath.Join(s.Cfg.DataDir, "catalog.json") }

// New loads (or discovers) the catalog and starts every subsystem.
func New(ctx context.Context, cfg *config.Config) (*Service, error) {
	s := &Service{Cfg: cfg, Ledger: cost.NewLedger(), Now: func() time.Time { return time.Now().UTC() }}
	cat, err := catalog.Load(s.catalogPath())
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	if cat == nil {
		if cat, _, err = s.refresh(ctx, nil); err != nil {
			return nil, err
		}
	}
	s.cat = cat

	s.Engines = map[string]engine.Engine{}
	needDuck, needOS := false, false
	for _, src := range cfg.Sources {
		needDuck = needDuck || src.Kind == "parquet" || src.Kind == "ndjson"
		needOS = needOS || src.Kind == "opensearch"
	}
	if needDuck {
		db, err := duckdb.Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		s.Engines["parquet"] = engine.NewRunner(duckdb.New(db, "parquet"))
		s.Engines["ndjson"] = engine.NewRunner(duckdb.New(db, "ndjson"))
	}
	if needOS {
		s.Engines["opensearch"] = engine.NewRunner(opensearch.New(cfg))
	}

	store, err := enrich.Open(cfg.Context)
	if err != nil {
		return nil, fmt.Errorf("context store: %w", err)
	}
	if store != nil {
		s.Enricher = enrich.New(store)
	}
	if s.Audit, err = audit.Open(filepath.Join(cfg.DataDir, "audit.jsonl")); err != nil {
		return nil, err
	}
	s.Jobs = exec.NewManager(cfg, s.Engines, s.Enricher, s.Ledger, s)
	s.Jobs.OnFinish = s.onFinish
	return s, nil
}

// Catalog returns the current catalog snapshot.
func (s *Service) Catalog() *catalog.Catalog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cat
}

// schema.Lookup delegation (so the executor can see field roles).
func (s *Service) HasDataset(d string) bool { return s.Catalog().HasDataset(d) }
func (s *Service) Field(d, p string) (schema.FieldType, schema.Role, bool) {
	return s.Catalog().Field(d, p)
}
func (s *Service) Fields(d string) []string { return s.Catalog().Fields(d) }

// Refresh rediscovers every location and saves the catalog atomically.
func (s *Service) Refresh(ctx context.Context) (*catalog.Report, error) {
	cat, rep, err := s.refresh(ctx, s.Catalog())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cat = cat
	s.mu.Unlock()
	return rep, nil
}

func (s *Service) refresh(ctx context.Context, prev *catalog.Catalog) (*catalog.Catalog, *catalog.Report, error) {
	cat, rep, err := catalog.Refresh(ctx, s.Cfg, prev)
	if err != nil {
		return nil, nil, err
	}
	if len(cat.Locations) == 0 {
		return nil, rep, fmt.Errorf("catalog refresh found no locations: %v", rep.Errors)
	}
	if err := catalog.Save(s.catalogPath(), cat); err != nil {
		return nil, nil, err
	}
	return cat, rep, nil
}

// Prepare normalizes and validates a query.
func (s *Service) Prepare(q *ir.Query) error {
	cat := s.Catalog()
	ir.Normalize(q, cat)
	return ir.Validate(q, cat)
}

// ParseQuery decodes IR JSON, then prepares it.
func (s *Service) ParseQuery(raw []byte) (*ir.Query, error) {
	var q ir.Query
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, &ir.ValidationError{Problems: []string{"malformed IR JSON: " + err.Error()}}
	}
	if err := s.Prepare(&q); err != nil {
		return nil, err
	}
	return &q, nil
}

// Plan slices, prunes, compiles and prices a prepared query. Planning is free
// and side-effect free.
func (s *Service) Plan(ctx context.Context, q *ir.Query) (*planner.Plan, error) {
	return s.PlanWith(ctx, q, false)
}

// PlanWith optionally disables overlap ownership (naive mode, for demos).
func (s *Service) PlanWith(ctx context.Context, q *ir.Query, naive bool) (*planner.Plan, error) {
	p := &planner.Planner{Cat: s.Catalog(), Cfg: s.Cfg, Now: s.Now, Lister: catalog.ListLive, Naive: naive}
	plan, err := p.Plan(ctx, q)
	if err != nil {
		return nil, err
	}
	est := map[string]cost.Estimator{}
	for k, e := range s.Engines {
		est[k] = e
	}
	if err := cost.Apply(ctx, plan, est); err != nil {
		return nil, err
	}
	return plan, nil
}

// Decide applies budget guardrails for a principal.
func (s *Service) Decide(p *planner.Plan, principal string, perHour float64) cost.Decision {
	return cost.Check(s.Cfg.Budget, perHour, s.Ledger, principal, p.Total.USD)
}

// SubmitOptions control one execution.
type SubmitOptions struct {
	Principal   string
	Surface     string
	PerHourUSD  float64 // principal-specific budget (agents)
	AutoConfirm bool
}

// Submit plans-and-runs: budget check, job creation, audit.
func (s *Service) Submit(ctx context.Context, p *planner.Plan, o SubmitOptions) (*exec.Job, error) {
	d := s.Decide(p, o.Principal, o.PerHourUSD)
	rec := audit.Record{Principal: o.Principal, Surface: o.Surface, QueryHash: p.ID, Dataset: p.Query.Dataset, USD: p.Total.USD, Bytes: p.Total.Bytes}
	for _, sl := range p.Slices {
		rec.Slices = append(rec.Slices, sl.Location)
	}
	if !d.Allowed {
		rec.Action, rec.Outcome, rec.Detail = "deny", "budget_denied", d.Reason
		s.Audit.Write(rec)
		return nil, fmt.Errorf("budget: %s", d.Reason)
	}
	j, err := s.Jobs.Submit(p, o.Principal, d, o.AutoConfirm)
	if err != nil {
		return nil, err
	}
	rec.Action, rec.JobID, rec.Outcome = "submit", j.ID, string(j.Snapshot().State)
	s.Audit.Write(rec)
	return j, nil
}

func (s *Service) onFinish(j *exec.Job) {
	snap := j.Snapshot()
	rec := audit.Record{Principal: snap.Principal, Action: "complete", JobID: snap.ID, QueryHash: snap.Plan.ID,
		Dataset: snap.Plan.Query.Dataset, Outcome: string(snap.State)}
	if r := snap.Result; r != nil {
		rec.Bytes, rec.USD, rec.LatencyMS, rec.Detail = r.Bytes, r.USD, r.Elapsed.Milliseconds(), r.Reason
		for _, sl := range r.Slices {
			rec.Slices = append(rec.Slices, sl.Location+":"+sl.State)
		}
	}
	s.Audit.Write(rec)
}

// Run is the synchronous path: prepare, plan, submit, wait.
func (s *Service) Run(ctx context.Context, q *ir.Query, o SubmitOptions) (*exec.Job, error) {
	if err := s.Prepare(q); err != nil {
		return nil, err
	}
	p, err := s.Plan(ctx, q)
	if err != nil {
		return nil, err
	}
	j, err := s.Submit(ctx, p, o)
	if err != nil {
		return nil, err
	}
	select {
	case <-j.Done():
	case <-ctx.Done():
		_ = s.Jobs.Cancel(j.ID)
		<-j.Done()
	}
	return j, nil
}
