// Package result defines what engines return and the merge layer combines:
// rows already mapped to OCSF paths, and mergeable partial aggregate states.
package result

import (
	"fmt"
	"sort"
	"time"

	"github.com/rksurwase/fedsearch/internal/ir"
)

// Row is one event, in OCSF terms, with provenance.
type Row struct {
	Time     time.Time      `json:"time"`
	ID       string         `json:"id"`       // event identity (metadata.uid)
	Location string         `json:"location"` // which physical copy it came from
	Slice    string         `json:"slice"`
	Fields   map[string]any `json:"fields"`
	Context  map[string]any `json:"context,omitempty"` // enrichment, keyed by field path
	Flags    []string       `json:"flags,omitempty"`   // e.g. "suspicious:http_request.user_agent"
}

// Get implements ir.Getter over the row's OCSF fields.
func (r *Row) Get(path string) (any, bool) {
	if path == "time" {
		return r.Time, true
	}
	if path == "metadata.uid" {
		return r.ID, true
	}
	v, ok := r.Fields[path]
	return v, ok
}

// Group is one aggregate bucket: group-by key values plus partial states,
// index-aligned with the query's aggs.
type Group struct {
	Key    []any    `json:"key"`
	States []*State `json:"states"`
}

func (g *Group) KeyString() string { return fmt.Sprint(g.Key...) }

// DistinctCap bounds the value set a count_distinct state carries.
const DistinctCap = 10_000

// State is a mergeable partial aggregate. Only decomposable functions are
// allowed, so merging partials from disjoint slices is exact (count_distinct
// is exact until DistinctCap, then marked approximate).
type State struct {
	Fn     ir.AggFn            `json:"fn"`
	Count  int64               `json:"count"`
	Sum    float64             `json:"sum"`
	Min    any                 `json:"min,omitempty"`
	Max    any                 `json:"max,omitempty"`
	Values map[string]struct{} `json:"-"`
	Capped bool                `json:"capped,omitempty"`
}

func NewState(fn ir.AggFn) *State {
	s := &State{Fn: fn}
	if fn == ir.CountDistinct {
		s.Values = map[string]struct{}{}
	}
	return s
}

// Merge folds another partial state into s.
func (s *State) Merge(o *State) {
	s.Count += o.Count
	s.Sum += o.Sum
	if o.Min != nil && (s.Min == nil || ir.Compare(o.Min, s.Min) < 0) {
		s.Min = o.Min
	}
	if o.Max != nil && (s.Max == nil || ir.Compare(o.Max, s.Max) > 0) {
		s.Max = o.Max
	}
	for v := range o.Values {
		if len(s.Values) >= DistinctCap {
			s.Capped = true
			break
		}
		s.Values[v] = struct{}{}
	}
	s.Capped = s.Capped || o.Capped
}

// Final returns the aggregate's value.
func (s *State) Final() any {
	switch s.Fn {
	case ir.Count:
		return s.Count
	case ir.Sum:
		if s.Sum == float64(int64(s.Sum)) {
			return int64(s.Sum)
		}
		return s.Sum
	case ir.Min:
		return s.Min
	case ir.Max:
		return s.Max
	case ir.Avg:
		if s.Count == 0 {
			return nil
		}
		return s.Sum / float64(s.Count)
	case ir.CountDistinct:
		return int64(len(s.Values))
	}
	return nil
}

// FinalGroup is a merged, finalized aggregate row.
type FinalGroup struct {
	Values map[string]any `json:"values"` // group fields and agg aliases
	Approx bool           `json:"approx,omitempty"`
}

// Finalize turns merged groups into named values and applies HAVING, order
// and limit.
func Finalize(q *ir.Query, groups []*Group) []FinalGroup {
	out := make([]FinalGroup, 0, len(groups))
	for _, g := range groups {
		fg := FinalGroup{Values: map[string]any{}}
		for i, f := range q.GroupBy {
			fg.Values[f] = g.Key[i]
		}
		for i, a := range q.Aggs {
			fg.Values[a.As] = g.States[i].Final()
			fg.Approx = fg.Approx || g.States[i].Capped
		}
		if q.Having != nil && !ir.Eval(q.Having, func(p string) (any, bool) { v, ok := fg.Values[p]; return v, ok }) {
			continue
		}
		out = append(out, fg)
	}
	by, desc := q.Aggs[0].As, true
	if q.Order != nil {
		by, desc = q.Order.By, q.Order.Desc
	}
	sort.SliceStable(out, func(i, j int) bool {
		c := ir.Compare(out[i].Values[by], out[j].Values[by])
		if c == 0 {
			return fmt.Sprint(out[i].Values) < fmt.Sprint(out[j].Values)
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}
