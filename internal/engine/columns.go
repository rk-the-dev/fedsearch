package engine

import (
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
)

// NeededPhysical returns the physical columns a slice reads: projected
// columns, group/agg fields, pushed predicates, time and identity. This is
// what a columnar engine actually scans, and what the byte accounting prices.
func NeededPhysical(s *planner.Slice, q *ir.Query) []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if b := s.Loc.Bindings[path]; b != nil && !seen[b.Physical] {
			seen[b.Physical] = true
			out = append(out, b.Physical)
		}
	}
	add("time")
	if !q.IsAggregate() {
		add("metadata.uid")
	}
	for _, c := range s.Columns {
		add(c)
	}
	for _, g := range q.GroupBy {
		add(g)
	}
	for _, a := range q.Aggs {
		add(a.Field)
	}
	s.Pushed.Walk(func(c *ir.Cmp) { add(c.Field) })
	return out
}
