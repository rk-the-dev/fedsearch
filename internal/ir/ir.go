// Package ir defines the typed query that every caller (UI, NL layer, MCP
// agents) produces and every engine consumes. No engine ever receives text an
// LLM wrote: the LLM emits IR, deterministic code validates it, and compilers
// turn it into native queries.
package ir

import "github.com/rksurwase/fedsearch/internal/timex"

// Version is the current IR schema version.
const Version = 1

// Query is one federated question over one logical dataset.
type Query struct {
	V       int       `json:"v"`
	Dataset string    `json:"dataset"`
	Time    TimeRange `json:"time"`
	Where   *Expr     `json:"where,omitempty"`
	Select  []string  `json:"select,omitempty"`   // row queries: OCSF paths to return
	GroupBy []string  `json:"group_by,omitempty"` // aggregate queries
	Aggs    []Agg     `json:"aggs,omitempty"`
	Having  *Expr     `json:"having,omitempty"` // over agg aliases and group fields
	Order   *Order    `json:"order,omitempty"`
	Limit   int       `json:"limit"`
	Enrich  []string  `json:"enrich,omitempty"` // OCSF paths of IP fields to enrich
}

// IsAggregate reports whether the query returns groups rather than rows.
func (q *Query) IsAggregate() bool { return len(q.Aggs) > 0 }

// TimeRange is a half-open [from, to) range. In JSON, endpoints may be
// RFC3339 timestamps, dates, or relative expressions such as "now-30d".
type TimeRange struct{ timex.Range }

// Expr is a boolean tree; exactly one branch is set.
type Expr struct {
	And []*Expr `json:"and,omitempty"`
	Or  []*Expr `json:"or,omitempty"`
	Not *Expr   `json:"not,omitempty"`
	Cmp *Cmp    `json:"cmp,omitempty"`
}

// Cmp compares one field with a literal.
type Cmp struct {
	Field string `json:"field"`
	Op    Op     `json:"op"`
	Value any    `json:"value,omitempty"`
}

type Op string

const (
	Eq     Op = "eq"
	Ne     Op = "ne"
	Lt     Op = "lt"
	Lte    Op = "lte"
	Gt     Op = "gt"
	Gte    Op = "gte"
	In     Op = "in"
	CIDR   Op = "cidr"   // ip field within a CIDR block
	Prefix Op = "prefix" // string starts with
	Exists Op = "exists" // field is not null
)

// AggFn is restricted to decomposable aggregates so partial results from
// disjoint time slices merge exactly.
type AggFn string

const (
	Count         AggFn = "count"
	Sum           AggFn = "sum"
	Min           AggFn = "min"
	Max           AggFn = "max"
	Avg           AggFn = "avg"            // carried as (sum, count)
	CountDistinct AggFn = "count_distinct" // carried as a capped value set
)

type Agg struct {
	Fn    AggFn  `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as"`
}

// Order sorts rows by time, or groups by an alias or group field.
type Order struct {
	By   string `json:"by"`
	Desc bool   `json:"desc,omitempty"`
}

// Helpers for building queries in code and tests.

func And(e ...*Expr) *Expr                   { return &Expr{And: e} }
func Or(e ...*Expr) *Expr                    { return &Expr{Or: e} }
func Not(e *Expr) *Expr                      { return &Expr{Not: e} }
func C(field string, op Op, value any) *Expr { return &Expr{Cmp: &Cmp{Field: field, Op: op, Value: value}} }

// Walk visits every comparison in the tree.
func (e *Expr) Walk(fn func(*Cmp)) {
	if e == nil {
		return
	}
	for _, c := range e.And {
		c.Walk(fn)
	}
	for _, c := range e.Or {
		c.Walk(fn)
	}
	e.Not.Walk(fn)
	if e.Cmp != nil {
		fn(e.Cmp)
	}
}

// Fields returns every OCSF path the query reads, deduplicated, in order of
// first appearance.
func (q *Query) Fields() []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range q.Select {
		add(f)
	}
	for _, f := range q.GroupBy {
		add(f)
	}
	for _, a := range q.Aggs {
		add(a.Field)
	}
	q.Where.Walk(func(c *Cmp) { add(c.Field) })
	for _, f := range q.Enrich {
		add(f)
	}
	return out
}
