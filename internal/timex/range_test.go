package timex

import (
	"testing"
	"time"
)

func d(s string) time.Time { t, _ := time.Parse("2006-01-02T15:04", s); return t }

func TestOverlaps(t *testing.T) {
	a := New(d("2026-09-01T00:00"), d("2026-09-10T00:00"))
	cases := []struct {
		name string
		b    Range
		want bool
	}{
		{"disjoint", New(d("2026-09-11T00:00"), d("2026-09-12T00:00")), false},
		{"touching is not overlap", New(d("2026-09-10T00:00"), d("2026-09-12T00:00")), false},
		{"nested", New(d("2026-09-02T00:00"), d("2026-09-03T00:00")), true},
		{"identical", a, true},
		{"zero length", New(d("2026-09-05T00:00"), d("2026-09-05T00:00")), false},
	}
	for _, c := range cases {
		if got := a.Overlaps(c.b); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestIntersectSubtract(t *testing.T) {
	q := New(d("2026-09-01T00:00"), d("2026-09-30T00:00"))
	hot := New(d("2026-09-07T00:00"), d("2026-10-07T00:00"))
	in, ok := q.Intersect(hot)
	if !ok || !in.From.Equal(d("2026-09-07T00:00")) || !in.To.Equal(q.To) {
		t.Fatalf("intersect = %v", in)
	}
	rest := q.Subtract(hot)
	if len(rest) != 1 || !rest[0].To.Equal(d("2026-09-07T00:00")) {
		t.Fatalf("subtract = %v", rest)
	}
	mid := New(d("2026-09-10T00:00"), d("2026-09-12T00:00"))
	if parts := q.Subtract(mid); len(parts) != 2 {
		t.Fatalf("subtract middle = %v", parts)
	}
}

func TestAlign(t *testing.T) {
	day := 24 * time.Hour
	if got := AlignUp(d("2026-09-06T23:59"), day); !got.Equal(d("2026-09-07T00:00")) {
		t.Fatalf("align up = %v", got)
	}
	if got := AlignUp(d("2026-09-07T00:00"), day); !got.Equal(d("2026-09-07T00:00")) {
		t.Fatalf("already aligned moved: %v", got)
	}
	if got := AlignDown(d("2026-09-07T13:00"), day); !got.Equal(d("2026-09-07T00:00")) {
		t.Fatalf("align down = %v", got)
	}
}
