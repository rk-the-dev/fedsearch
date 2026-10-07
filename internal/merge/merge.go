// Package merge combines per-slice results into one answer.
//
// Rows: a k-way merge over time-sorted slice streams. Because duplicates of
// an event share an identical timestamp, they arrive adjacent in merge order,
// so dedup needs only the set of IDs seen at the current timestamp: O(1)
// memory per timestamp instead of a set of every ID.
//
// Aggregates: partial states keyed by group, merged with State.Merge. This is
// only exact because the planner guarantees disjoint slices — aggregate
// partials cannot be deduplicated after the fact.
package merge

import (
	"container/heap"

	"github.com/rksurwase/fedsearch/internal/result"
)

// RowStats reports what the merge did.
type RowStats struct {
	Input      int `json:"input"`
	Duplicates int `json:"duplicates"` // dropped by event identity
	Output     int `json:"output"`
}

type cursor struct {
	rows []result.Row
	pos  int
}

type rowHeap struct {
	cur  []*cursor
	desc bool
}

func (h rowHeap) Len() int { return len(h.cur) }
func (h rowHeap) Less(i, j int) bool {
	a, b := h.cur[i].rows[h.cur[i].pos], h.cur[j].rows[h.cur[j].pos]
	if !a.Time.Equal(b.Time) {
		if h.desc {
			return a.Time.After(b.Time)
		}
		return a.Time.Before(b.Time)
	}
	return a.ID < b.ID
}
func (h rowHeap) Swap(i, j int) { h.cur[i], h.cur[j] = h.cur[j], h.cur[i] }
func (h *rowHeap) Push(x any)   { h.cur = append(h.cur, x.(*cursor)) }
func (h *rowHeap) Pop() any {
	old := h.cur
	n := len(old)
	x := old[n-1]
	h.cur = old[:n-1]
	return x
}

// Rows merges time-sorted streams, drops duplicate events, and stops at limit.
func Rows(streams [][]result.Row, desc bool, limit int) ([]result.Row, RowStats) {
	h := &rowHeap{desc: desc}
	var st RowStats
	for _, s := range streams {
		st.Input += len(s)
		if len(s) > 0 {
			h.cur = append(h.cur, &cursor{rows: s})
		}
	}
	heap.Init(h)
	var out []result.Row
	var curTime int64 = -1 << 62
	seenAtTime := map[string]bool{}
	for h.Len() > 0 && len(out) < limit {
		c := h.cur[0]
		r := c.rows[c.pos]
		c.pos++
		if c.pos == len(c.rows) {
			heap.Pop(h)
		} else {
			heap.Fix(h, 0)
		}
		if t := r.Time.UnixNano(); t != curTime {
			curTime = t
			clear(seenAtTime)
		}
		if seenAtTime[r.ID] {
			st.Duplicates++
			continue
		}
		seenAtTime[r.ID] = true
		out = append(out, r)
	}
	st.Output = len(out)
	return out, st
}

// Groups merges partial groups from every slice into one group per key.
func Groups(parts [][]*result.Group) []*result.Group {
	byKey := map[string]*result.Group{}
	var order []string
	for _, slice := range parts {
		for _, g := range slice {
			k := g.KeyString()
			if cur, ok := byKey[k]; ok {
				for i := range cur.States {
					cur.States[i].Merge(g.States[i])
				}
				continue
			}
			byKey[k] = g
			order = append(order, k)
		}
	}
	out := make([]*result.Group, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}
