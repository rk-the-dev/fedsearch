// Package e2e runs the whole system in lite mode (Parquet + NDJSON via
// DuckDB, CSV context) against the generator's ground truth. Run `make gen`
// first; tests skip when the data is missing.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/enrich"
	"github.com/rksurwase/fedsearch/internal/exec"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/service"
)

type groundTruth struct {
	Now       time.Time `json:"now"`
	HotFrom   time.Time `json:"hot_from"`
	ColdUntil time.Time `json:"cold_until"`
	Stages    []struct {
		Name   string    `json:"name"`
		Start  time.Time `json:"start"`
		Events int       `json:"events"`
	} `json:"attack_stages"`
}

var (
	once   sync.Once
	svc    *service.Service
	gt     groundTruth
	setupE error
)

func setup(t *testing.T) (*service.Service, groundTruth) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	b, err := os.ReadFile(filepath.Join(root, "out", "ground_truth.json"))
	if err != nil {
		t.Skip("no generated data; run `make gen`")
	}
	once.Do(func() {
		if setupE = json.Unmarshal(b, &gt); setupE != nil {
			return
		}
		var cfg *config.Config
		if cfg, setupE = config.Load(filepath.Join(root, "deploy", "lite.json")); setupE != nil {
			return
		}
		dir, _ := os.MkdirTemp("", "fedsearch-e2e-")
		cfg.DataDir = dir
		now := gt.Now.Add(10 * time.Hour)
		ir.Now = func() time.Time { return now }
		svc, setupE = service.New(context.Background(), cfg)
		if svc != nil {
			svc.Now = func() time.Time { return now }
		}
	})
	if setupE != nil {
		t.Fatal(setupE)
	}
	return svc, gt
}

func run(t *testing.T, s *service.Service, irJSON string) *exec.Job {
	t.Helper()
	q, err := s.ParseQuery([]byte(irJSON))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	j, err := s.Run(ctx, q, service.SubmitOptions{Principal: "test", Surface: "test", AutoConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	snap := j.Snapshot()
	if snap.Result.Status != exec.Completed {
		t.Fatalf("job %s: %s (%s) slices=%+v", snap.ID, snap.Result.Status, snap.Result.Reason, snap.Result.Slices)
	}
	return j
}

func count(t *testing.T, j *exec.Job) int64 {
	t.Helper()
	g := j.Snapshot().Result.Groups
	if len(g) != 1 {
		t.Fatalf("groups = %d", len(g))
	}
	return g[0].Values["n"].(int64)
}

// The brute-force burst lives in the hot/cold overlap window. Disjoint slices
// must count each event once: 40, not 80.
func TestBruteForceCountedOnceAcrossTiers(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]},
		"aggs":[{"fn":"count","as":"n"}]}`)
	if n := count(t, j); n != 40 {
		t.Fatalf("failures = %d, want 40", n)
	}
	p := j.Snapshot().Plan
	if len(p.Slices) != 2 {
		t.Fatalf("slices = %d, want 2 (cold + hot)", len(p.Slices))
	}
	if !p.Slices[0].Time.To.Equal(p.Slices[1].Time.From) {
		t.Fatalf("slices not contiguous: %s / %s", p.Slices[0].Time, p.Slices[1].Time)
	}
	if !strings.Contains(p.Slices[0].Native, "'failure'") {
		t.Errorf("enum transform not applied in SQL:\n%s", p.Slices[0].Native)
	}
}

// Naive mode (no overlap ownership) demonstrates the bug the design prevents.
func TestNaiveModeDoubleCounts(t *testing.T) {
	s, _ := setup(t)
	q, _ := s.ParseQuery([]byte(`{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]},
		"aggs":[{"fn":"count","as":"n"}]}`))
	p, err := s.PlanWith(context.Background(), q, true)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Submit(context.Background(), p, service.SubmitOptions{Principal: "test", AutoConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	<-j.Done()
	if n := count(t, j); n != 80 {
		t.Fatalf("naive count = %d, want 80 (double counted)", n)
	}
	// Row queries survive naive mode thanks to dedup by event identity.
	q2, _ := s.ParseQuery([]byte(`{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]},"limit":500}`))
	p2, _ := s.PlanWith(context.Background(), q2, true)
	j2, _ := s.Submit(context.Background(), p2, service.SubmitOptions{Principal: "test", AutoConfirm: true})
	<-j2.Done()
	r := j2.Snapshot().Result
	if len(r.Rows) != 40 || r.Merge.Duplicates != 40 {
		t.Fatalf("naive rows=%d dupes=%d, want 40/40", len(r.Rows), r.Merge.Duplicates)
	}
}

func TestReconOnlyInColdTier(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"network_activity","time":{"from":"now-180d"},
		"where":{"cmp":{"field":"src_endpoint.ip","op":"eq","value":"185.220.101.47"}},
		"group_by":["action_id"],"aggs":[{"fn":"count","as":"n"}]}`)
	g := j.Snapshot().Result.Groups
	if len(g) != 1 || g[0].Values["n"].(int64) != 300 || g[0].Values["action_id"].(int64) != 2 {
		t.Fatalf("recon groups = %+v, want 300 denied", g)
	}
}

func TestExfilOnlyInHotTier(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"network_activity","time":{"from":"now-180d"},
		"where":{"cmp":{"field":"dst_endpoint.ip","op":"eq","value":"185.220.101.47"}},
		"aggs":[{"fn":"count","as":"n"},{"fn":"sum","field":"traffic.bytes_out","as":"bytes"},{"fn":"count_distinct","field":"src_endpoint.ip","as":"sources"}]}`)
	v := j.Snapshot().Result.Groups[0].Values
	if v["n"].(int64) != 30 || v["sources"].(int64) != 1 {
		t.Fatalf("exfil = %+v", v)
	}
	if b := v["bytes"].(int64); b < 3.6e9 || b > 4.8e9 {
		t.Fatalf("exfil bytes = %d", b)
	}
}

// As-of enrichment attributes the lateral movement to the laptop that held
// the IP at the time, not the one holding it today.
func TestAsOfEnrichment(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"dst_endpoint.hostname","op":"eq","value":"db-prod-01"}}]},
		"enrich":["src_endpoint.ip"]}`)
	rows := j.Snapshot().Result.Rows
	if len(rows) != 1 {
		t.Fatalf("lateral rows = %d", len(rows))
	}
	c, ok := rows[0].Context["src_endpoint.ip"].(enrich.Context)
	if !ok || c.AsOf == nil || c.AsOf.Hostname != "lt-ankit-042" || c.Current.Hostname != "lt-meera-118" || !c.Changed {
		t.Fatalf("context = %+v", rows[0].Context)
	}
}

// DuckDB cannot evaluate CIDR natively: rows are filtered after fetch
// (residual), aggregates are rejected rather than computed wrongly.
func TestResidualPredicates(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"src_endpoint.ip","op":"cidr","value":"10.20.0.0/16"}}]}}`)
	snap := j.Snapshot()
	if len(snap.Result.Rows) != 1 || snap.Plan.Slices[0].Residual == nil {
		t.Fatalf("rows=%d residual=%v", len(snap.Result.Rows), snap.Plan.Slices[0].Residual)
	}
	q, _ := s.ParseQuery([]byte(`{"dataset":"authentication","time":{"from":"now-30d"},
		"where":{"cmp":{"field":"src_endpoint.ip","op":"cidr","value":"10.20.0.0/16"}},"aggs":[{"fn":"count","as":"n"}]}`))
	_, err := s.Plan(context.Background(), q)
	var rae *planner.ResidualAggregateError
	if !errors.As(err, &rae) {
		t.Fatalf("expected ResidualAggregateError, got %v", err)
	}
}

func TestInjectionFlagged(t *testing.T) {
	s, _ := setup(t)
	j := run(t, s, `{"dataset":"authentication","time":{"from":"now-180d"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]},"limit":100}`)
	if f := j.Snapshot().Result.Flagged; f != 1 {
		t.Fatalf("flagged = %d, want 1", f)
	}
}

// Recent windows touch only the hot tier; the cold slice and its partitions
// are pruned away, and cold actual bytes never exceed the estimate.
func TestPruningAndCost(t *testing.T) {
	s, _ := setup(t)
	q, _ := s.ParseQuery([]byte(`{"dataset":"network_activity","time":{"from":"now-3d"},"aggs":[{"fn":"count","as":"n"}]}`))
	p, err := s.Plan(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Slices) != 1 || p.Slices[0].Tier != "hot" || len(p.Slices[0].Partitions) > 4 {
		t.Fatalf("recent query plan: %d slices, tier %s, %d partitions", len(p.Slices), p.Slices[0].Tier, len(p.Slices[0].Partitions))
	}
	j := run(t, s, `{"dataset":"network_activity","time":{"from":"now-120d","to":"now-40d"},"group_by":["dst_endpoint.port"],"aggs":[{"fn":"sum","field":"traffic.bytes_out","as":"out"}]}`)
	snap := j.Snapshot()
	sl := snap.Plan.Slices[0]
	act := snap.Result.Slices[0].Bytes
	if sl.Tier != "cold" || act <= 0 || act > sl.Estimate.Bytes {
		t.Fatalf("cold slice estimate=%d actual=%d", sl.Estimate.Bytes, act)
	}
	t.Logf("cold: %d of %d partitions, estimate %d B, actual %d B, file bytes %d", len(sl.Partitions), sl.PartitionsTotal, sl.Estimate.Bytes, act, sumBytes(sl))
}

func sumBytes(s *planner.Slice) int64 {
	var b int64
	for _, p := range s.Partitions {
		b += p.Bytes
	}
	return b
}
