package datagen

import (
	"math/rand"
	"testing"
	"time"
)

var anchor = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

func newTestWorld() *World { return NewWorld(rand.New(rand.NewSource(42)), anchor, 180, 50) }

// An IP must never belong to two hosts at once. Postgres enforces the same
// rule with an exclusion constraint; this keeps the generator honest.
func TestAssignmentsDoNotOverlap(t *testing.T) {
	w := newTestWorld()
	for i := 1; i < len(w.Assignments); i++ {
		a, b := w.Assignments[i-1], w.Assignments[i]
		if a.IP != b.IP {
			continue
		}
		if a.To.IsZero() || b.From.Before(a.To) {
			t.Fatalf("overlap on %s: %s [%s,%s) vs %s [%s,…)", a.IP, a.Host, a.From, a.To, b.Host, b.From)
		}
	}
}

// The core enrichment trap: the reused IP means different hosts over time.
func TestAsOfAttribution(t *testing.T) {
	w := newTestWorld()
	lateral := anchor.AddDate(0, 0, -27)
	if got := w.HostOf(ReusedIP, lateral); got != CompromisedHst {
		t.Fatalf("as-of host for %s at lateral time = %q, want %q", ReusedIP, got, CompromisedHst)
	}
	if got := w.HostOf(ReusedIP, anchor); got != ReassignedHst {
		t.Fatalf("current host for %s = %q, want %q (the trap)", ReusedIP, got, ReassignedHst)
	}
	if got := w.HostOf(AnkitNewIP, anchor.AddDate(0, 0, -10)); got != CompromisedHst {
		t.Fatalf("exfil source attribution = %q, want %q", got, CompromisedHst)
	}
}

func TestTierWindows(t *testing.T) {
	tr := Tiers{Now: anchor, HotDays: 30, ArchiveLagDays: 23}
	cases := []struct {
		daysAgo   int
		hot, cold bool
	}{
		{45, false, true}, // recon: cold only
		{27, true, true},  // brute force: overlap
		{10, true, false}, // exfil: hot only
	}
	for _, c := range cases {
		d := anchor.AddDate(0, 0, -c.daysAgo)
		if tr.InHot(d) != c.hot || tr.InCold(d) != c.cold {
			t.Errorf("%d days ago: hot=%v cold=%v, want hot=%v cold=%v", c.daysAgo, tr.InHot(d), tr.InCold(d), c.hot, c.cold)
		}
	}
}

func TestDeterministic(t *testing.T) {
	var s1, s2 int64
	a1, _, _ := newTestWorld().Attack(rand.New(rand.NewSource(1)), &s1)
	a2, _, _ := newTestWorld().Attack(rand.New(rand.NewSource(1)), &s2)
	if len(a1) != len(a2) || a1[0].EventID != a2[0].EventID {
		t.Fatal("same seed produced different attack events")
	}
}
