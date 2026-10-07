// Package timex holds the half-open time range shared by the catalog, the IR
// and the planner. Defining it once keeps interval semantics consistent: a
// range is [From, To), so adjacent ranges touch but never overlap.
package timex

import (
	"fmt"
	"time"
)

// Range is the half-open interval [From, To).
type Range struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func New(from, to time.Time) Range { return Range{From: from.UTC(), To: to.UTC()} }

// IsZero reports whether the range is empty (To <= From).
func (r Range) IsZero() bool { return !r.From.Before(r.To) }

func (r Range) Duration() time.Duration {
	if r.IsZero() {
		return 0
	}
	return r.To.Sub(r.From)
}

func (r Range) Contains(t time.Time) bool { return !t.Before(r.From) && t.Before(r.To) }

func (r Range) Overlaps(o Range) bool {
	return !r.IsZero() && !o.IsZero() && r.From.Before(o.To) && o.From.Before(r.To)
}

// Intersect returns the shared part of r and o.
func (r Range) Intersect(o Range) (Range, bool) {
	from, to := r.From, r.To
	if o.From.After(from) {
		from = o.From
	}
	if o.To.Before(to) {
		to = o.To
	}
	out := Range{From: from, To: to}
	return out, !out.IsZero()
}

// Subtract returns the parts of r not covered by o (zero, one or two ranges).
func (r Range) Subtract(o Range) []Range {
	if !r.Overlaps(o) {
		if r.IsZero() {
			return nil
		}
		return []Range{r}
	}
	var out []Range
	if r.From.Before(o.From) {
		out = append(out, Range{From: r.From, To: o.From})
	}
	if o.To.Before(r.To) {
		out = append(out, Range{From: o.To, To: r.To})
	}
	return out
}

func (r Range) String() string {
	return fmt.Sprintf("[%s, %s)", r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339))
}

// AlignDown truncates t to a multiple of g since the Unix epoch (UTC).
func AlignDown(t time.Time, g time.Duration) time.Time {
	if g <= 0 {
		return t
	}
	return t.UTC().Truncate(g)
}

// AlignUp rounds t up to the next multiple of g, or returns t if already aligned.
func AlignUp(t time.Time, g time.Duration) time.Time {
	d := AlignDown(t, g)
	if d.Equal(t.UTC()) {
		return d
	}
	return d.Add(g)
}
