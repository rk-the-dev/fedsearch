// Package compiler turns one planned slice of an IR query into a native query
// for one engine: DuckDB SQL (Parquet / NDJSON) or OpenSearch query DSL.
// Compilers are pure functions: no I/O, fully covered by golden tests.
//
// Every literal is translated through the location's field bindings, so an
// OCSF predicate such as status_id = 2 becomes status = 'failure' where the
// pipeline stored captions.
package compiler

import (
	"fmt"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/timex"
)

// Input is everything a compiler needs for one slice.
type Input struct {
	Query   *ir.Query
	Loc     *catalog.Location
	Time    timex.Range
	Pushed  *ir.Expr // predicates the engine evaluates
	Columns []string // OCSF paths each returned row must carry
	Limit   int      // rows per slice, or group cap for aggregates
	Files   []string // SQL engines: object URIs after partition pruning
	Indices []string // OpenSearch: index names after pruning
}

// Output columns of an aggregate query, per agg index.
type AggCols struct {
	Count, Sum, Min, Max, Distinct string
}

// GroupCap is how many groups each slice returns for an aggregate query with
// limit n: over-fetching makes the merged top-N exact unless a slice hits it.
func GroupCap(limit int) int {
	c := limit * 10
	if c > 10_000 {
		c = 10_000
	}
	return c
}

func binding(loc *catalog.Location, path string) (*catalog.FieldBinding, error) {
	b := loc.Bindings[path]
	if b == nil {
		return nil, fmt.Errorf("%s has no physical column for %s", loc.ID, path)
	}
	return b, nil
}

// physicalLiteral maps an OCSF literal through the binding's enum transform.
func physicalLiteral(b *catalog.FieldBinding, v any) (any, error) {
	pv, ok := b.ToPhysical(v)
	if !ok {
		return nil, fmt.Errorf("value %v has no physical representation in %s", v, b.Physical)
	}
	return pv, nil
}

// AggAlias returns the column alias of a partial for agg i.
func AggAlias(i int, part string) string { return fmt.Sprintf("a%d_%s", i, part) }

// Pushable reports whether a predicate subtree can be evaluated natively by
// a location: every field bound and searchable, every operator supported, and
// every enum literal translatable.
func Pushable(e *ir.Expr, loc *catalog.Location) bool {
	ok := true
	e.Walk(func(c *ir.Cmp) {
		b := loc.Bindings[c.Field]
		if b == nil || !b.Searchable || !loc.Caps.Supports(string(c.Op)) {
			ok = false
			return
		}
		if b.Enum != nil {
			switch c.Op {
			case ir.Eq, ir.Ne, ir.Exists:
				if c.Op != ir.Exists {
					if _, good := b.ToPhysical(c.Value); !good {
						ok = false
					}
				}
			case ir.In:
				list, _ := c.Value.([]any)
				for _, v := range list {
					if _, good := b.ToPhysical(v); !good {
						ok = false
					}
				}
			default: // range comparisons on captions are meaningless
				ok = false
			}
		}
	})
	return ok
}
