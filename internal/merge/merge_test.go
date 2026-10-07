package merge

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/result"
)

func row(sec int, id string) result.Row {
	return result.Row{Time: time.Unix(int64(sec), 0).UTC(), ID: id}
}

func TestRowsDedupAdjacentDuplicates(t *testing.T) {
	cold := []result.Row{row(1, "a"), row(2, "b"), row(3, "c")}
	hot := []result.Row{row(2, "b"), row(3, "c"), row(4, "d")} // overlap window duplicated
	out, st := Rows([][]result.Row{cold, hot}, false, 100)
	if st.Duplicates != 2 || len(out) != 4 {
		t.Fatalf("dupes=%d out=%d", st.Duplicates, len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Time.Before(out[i-1].Time) {
			t.Fatal("not time ordered")
		}
	}
}

func TestRowsDistinctEventsSameTimestampKept(t *testing.T) {
	out, st := Rows([][]result.Row{{row(5, "x"), row(5, "y")}, {row(5, "z")}}, false, 10)
	if len(out) != 3 || st.Duplicates != 0 {
		t.Fatalf("out=%d dupes=%d", len(out), st.Duplicates)
	}
}

func TestRowsDescAndLimit(t *testing.T) {
	out, _ := Rows([][]result.Row{{row(9, "a"), row(5, "b")}, {row(7, "c"), row(1, "d")}}, true, 3)
	got := []string{out[0].ID, out[1].ID, out[2].ID}
	if fmt.Sprint(got) != "[a c b]" {
		t.Fatalf("got %v", got)
	}
}

// Property: splitting a dataset into arbitrary disjoint time slices and
// merging the partial aggregates equals aggregating it in one place.
func TestGroupsMergeEqualsSingleSource(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	q := &ir.Query{GroupBy: []string{"user"}, Aggs: []ir.Agg{
		{Fn: ir.Count, As: "n"}, {Fn: ir.Sum, Field: "b", As: "s"}, {Fn: ir.Avg, Field: "b", As: "avg"},
		{Fn: ir.Min, Field: "b", As: "mn"}, {Fn: ir.Max, Field: "b", As: "mx"}, {Fn: ir.CountDistinct, Field: "ip", As: "d"},
	}, Limit: 100, Order: &ir.Order{By: "n", Desc: true}}
	type ev struct {
		user, ip string
		b        int64
	}
	var evs []ev
	for i := 0; i < 5000; i++ {
		evs = append(evs, ev{fmt.Sprintf("u%d", r.Intn(20)), fmt.Sprintf("10.0.0.%d", r.Intn(50)), int64(r.Intn(1000))})
	}
	partial := func(part []ev) []*result.Group {
		byUser := map[string]*result.Group{}
		for _, e := range part {
			g := byUser[e.user]
			if g == nil {
				g = &result.Group{Key: []any{e.user}}
				for _, a := range q.Aggs {
					g.States = append(g.States, result.NewState(a.Fn))
				}
				byUser[e.user] = g
			}
			for _, s := range g.States {
				one := result.NewState(s.Fn)
				one.Count, one.Sum, one.Min, one.Max = 1, float64(e.b), e.b, e.b
				if s.Fn == ir.CountDistinct {
					one.Values[e.ip] = struct{}{}
				}
				s.Merge(one)
			}
		}
		var out []*result.Group
		for _, g := range byUser {
			out = append(out, g)
		}
		return out
	}
	whole := result.Finalize(q, partial(evs))
	for trial := 0; trial < 20; trial++ {
		cuts := []int{0, len(evs)}
		for i := 0; i < 1+r.Intn(5); i++ {
			cuts = append(cuts, r.Intn(len(evs)))
		}
		sort.Ints(cuts)
		var parts [][]*result.Group
		for i := 1; i < len(cuts); i++ {
			parts = append(parts, partial(evs[cuts[i-1]:cuts[i]]))
		}
		merged := result.Finalize(q, Groups(parts))
		if fmt.Sprint(merged) != fmt.Sprint(whole) {
			t.Fatalf("trial %d: merged != whole\n%v\n%v", trial, merged, whole)
		}
	}
}
