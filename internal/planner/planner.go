// Package planner turns a validated IR query into disjoint, partition-pruned,
// compiled slices — one per physical location that will answer part of the
// time range.
//
// The central invariant: slices never overlap in time. Row duplicates could be
// removed after the fact by event identity, but aggregate partials cannot, so
// disjointness is what makes merged counts exact. The most preferred (hot)
// location owns any window that several locations hold, and split points are
// aligned to partition boundaries so the cold tier reads whole partitions.
package planner

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/compiler"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/timex"
)

type Plan struct {
	ID        string        `json:"id"` // canonical query hash
	Query     *ir.Query     `json:"query"`
	Slices    []*Slice      `json:"slices"`
	Gaps      []timex.Range `json:"gaps,omitempty"`
	Warnings  []string      `json:"warnings,omitempty"`
	Total     Estimate      `json:"total"`
	CreatedAt time.Time     `json:"created_at"`
}

type Slice struct {
	ID              string        `json:"id"`
	Location        string        `json:"location"`
	Source          string        `json:"source"`
	Kind            string        `json:"kind"`
	Tier            string        `json:"tier"`
	Dialect         string        `json:"dialect"`
	Time            timex.Range   `json:"time"`
	Partitions      []PartRef     `json:"partitions"`
	PartitionsTotal int           `json:"partitions_total"` // in the location, for the pruning ratio
	Columns         []string      `json:"columns"`
	Pushed          *ir.Expr      `json:"pushed,omitempty"`
	Residual        *ir.Expr      `json:"residual,omitempty"`
	EngineLimit     int           `json:"engine_limit"`
	Native          string        `json:"native"`
	Estimate        Estimate      `json:"estimate"`
	Timeout         time.Duration `json:"timeout_ns"`

	Files   []string             `json:"-"`
	Indices []string             `json:"-"`
	SQL     *compiler.SQL        `json:"-"`
	DSL     *compiler.DSL        `json:"-"`
	Loc     *catalog.Location    `json:"-"`
	Parts   []*catalog.Partition `json:"-"`
}

type PartRef struct {
	Key   string      `json:"key"`
	Span  timex.Range `json:"span"`
	Rows  int64       `json:"rows"`
	Bytes int64       `json:"bytes"`
	Stale bool        `json:"stale,omitempty"`
}

// Estimate is filled in by the cost package.
type Estimate struct {
	Bytes  int64   `json:"bytes"`
	Rows   int64   `json:"rows"`
	USD    float64 `json:"usd"`
	Method string  `json:"method,omitempty"`
}

// Lister lists partitions live at plan time (catalog.ListLive in production).
type Lister func(ctx context.Context, src config.Source, loc *catalog.Location, r timex.Range) ([]*catalog.Partition, error)

type Planner struct {
	Cat    *catalog.Catalog
	Cfg    *config.Config
	Now    func() time.Time
	Lister Lister
	// Naive disables overlap ownership: every location answers its whole
	// coverage. Exists only to demonstrate the double-counting it causes.
	Naive bool
}

// ResidualAggregateError is returned when an aggregate query needs a predicate
// an engine cannot evaluate: filtering after aggregation would be wrong.
type ResidualAggregateError struct {
	Location string
	Expr     *ir.Expr
}

func (e *ResidualAggregateError) Error() string {
	var fields []string
	e.Expr.Walk(func(c *ir.Cmp) { fields = append(fields, fmt.Sprintf("%s %s", c.Field, c.Op)) })
	return fmt.Sprintf("%s cannot evaluate %v natively; an aggregate cannot be filtered after the fact. Rewrite the predicate or run a row query", e.Location, fields)
}

// Plan slices, prunes and compiles a normalized, validated query.
func (p *Planner) Plan(ctx context.Context, q *ir.Query) (*Plan, error) {
	now := p.Now()
	plan := &Plan{ID: ir.Hash(q), Query: q, CreatedAt: now}
	locs := p.Cat.LocationsFor(q.Dataset)
	if len(locs) == 0 {
		return nil, fmt.Errorf("dataset %s has no locations", q.Dataset)
	}

	// 1. Assign disjoint time ranges by preference.
	remaining := []timex.Range{q.Time.Range}
	type claim struct {
		loc *catalog.Location
		r   timex.Range
	}
	var claims []claim
	for i, loc := range locs {
		cov := loc.Coverage.Effective(now)
		if i < len(locs)-1 {
			// Align the start up to the next location's partition width, so
			// the less-preferred location keeps whole partitions.
			cov.From = timex.AlignUp(cov.From, locs[i+1].Coverage.Granularity())
		}
		if p.Naive {
			if in, ok := q.Time.Range.Intersect(loc.Coverage.Effective(now)); ok {
				claims = append(claims, claim{loc, in})
			}
		}
		var next []timex.Range
		for _, r := range remaining {
			if in, ok := r.Intersect(cov); ok && !p.Naive {
				claims = append(claims, claim{loc, in})
			}
			next = append(next, r.Subtract(cov)...)
		}
		remaining = next
	}
	plan.Gaps = append(plan.Gaps, remaining...)
	if p.Naive {
		plan.Warnings = append(plan.Warnings, "naive mode: overlap ownership disabled — locations holding the same window are all queried (demonstration only)")
	}

	// 2. Per claim: live partition listing, pushdown split, compile.
	for _, c := range claims {
		src, ok := p.Cfg.Source(c.loc.Source)
		if !ok {
			return nil, fmt.Errorf("location %s: source %s not configured", c.loc.ID, c.loc.Source)
		}
		parts, err := p.Lister(ctx, src, c.loc, c.r)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: partition listing failed (%v); slice skipped", c.loc.ID, err))
			plan.Gaps = append(plan.Gaps, c.r)
			continue
		}
		if len(parts) == 0 {
			plan.Gaps = append(plan.Gaps, c.r)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: coverage claims %s but no partitions exist", c.loc.ID, c.r))
			continue
		}
		s, err := p.slice(q, c.loc, src, c.r, parts)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			if part.Stale {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: partition %s is newer than the catalog; estimate uses averages (stale_stats)", c.loc.ID, part.Key))
			}
		}
		plan.Slices = append(plan.Slices, s)
	}
	sort.Slice(plan.Slices, func(i, j int) bool { return plan.Slices[i].Time.From.Before(plan.Slices[j].Time.From) })
	for i, s := range plan.Slices {
		s.ID = fmt.Sprintf("s%d", i+1)
		if s.Residual != nil && !q.IsAggregate() {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %s filtered after fetch (not supported natively); over-fetching %d rows (may_truncate)", s.ID, s.Location, s.EngineLimit))
		}
	}
	for _, g := range plan.Gaps {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("no location covers %s: results for that window are missing (coverage_gap)", g))
	}
	if q.IsAggregate() {
		for _, a := range q.Aggs {
			if a.Fn == ir.CountDistinct {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s is exact up to %d values per group, approximate beyond", a.As, 10_000))
			}
		}
	}
	return plan, nil
}

func (p *Planner) slice(q *ir.Query, loc *catalog.Location, src config.Source, r timex.Range, parts []*catalog.Partition) (*Slice, error) {
	s := &Slice{
		Location: loc.ID, Source: loc.Source, Kind: loc.Kind, Tier: loc.Tier, Dialect: loc.Caps.Dialect,
		Time: r, PartitionsTotal: len(loc.Partitions), Loc: loc, Parts: parts, Timeout: src.Timeout(),
	}
	for _, part := range parts {
		s.Partitions = append(s.Partitions, PartRef{Key: part.Key, Span: part.Span, Rows: part.Rows, Bytes: part.Bytes, Stale: part.Stale})
		if loc.Kind == "opensearch" {
			s.Indices = append(s.Indices, part.Key)
		} else {
			s.Files = append(s.Files, part.Objects...)
		}
	}

	// Pushdown: each top-level conjunct is pushed whole or kept residual.
	var pushed, residual []*ir.Expr
	conj := []*ir.Expr{}
	if q.Where != nil {
		if len(q.Where.And) > 0 {
			conj = q.Where.And
		} else {
			conj = []*ir.Expr{q.Where}
		}
	}
	for _, c := range conj {
		if compiler.Pushable(c, loc) {
			pushed = append(pushed, c)
		} else {
			residual = append(residual, c)
		}
	}
	s.Pushed, s.Residual = andOf(pushed), andOf(residual)
	if s.Residual != nil && q.IsAggregate() {
		return nil, &ResidualAggregateError{Location: loc.ID, Expr: s.Residual}
	}

	if q.IsAggregate() {
		s.EngineLimit = compiler.GroupCap(q.Limit)
	} else {
		s.EngineLimit = q.Limit
		if s.Residual != nil {
			s.EngineLimit = min(q.Limit*10, ir.MaxRowLimit)
		}
		seen := map[string]bool{}
		add := func(f string) {
			if !seen[f] {
				seen[f] = true
				s.Columns = append(s.Columns, f)
			}
		}
		for _, f := range q.Select {
			add(f)
		}
		for _, f := range q.Enrich {
			add(f)
		}
		s.Residual.Walk(func(c *ir.Cmp) { add(c.Field) })
	}

	in := compiler.Input{Query: q, Loc: loc, Time: r, Pushed: s.Pushed, Columns: s.Columns, Limit: s.EngineLimit, Files: s.Files, Indices: s.Indices}
	switch loc.Caps.Dialect {
	case "duckdb_sql":
		sql, err := compiler.CompileSQL(in)
		if err != nil {
			return nil, err
		}
		s.SQL, s.Native = sql, sql.Text
	case "opensearch_dsl":
		dsl, err := compiler.CompileDSL(in)
		if err != nil {
			return nil, err
		}
		s.DSL, s.Native = dsl, dsl.Text()
	default:
		return nil, fmt.Errorf("no compiler for dialect %s", loc.Caps.Dialect)
	}
	return s, nil
}

func andOf(e []*ir.Expr) *ir.Expr {
	switch len(e) {
	case 0:
		return nil
	case 1:
		return e[0]
	}
	return &ir.Expr{And: e}
}
