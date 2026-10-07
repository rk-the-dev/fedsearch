// Command bench measures the claims the design makes and writes them to
// bench/results/<mode>.json and docs/benchmarks.md. Every number is
// reproducible with `make bench` (lite) or `make bench-docker`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/enrich"
	fexec "github.com/rksurwase/fedsearch/internal/exec"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/nl"
	"github.com/rksurwase/fedsearch/internal/service"
)

type metric struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Claim  string `json:"claim"`
	Pass   *bool  `json:"pass,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type report struct {
	Mode    string            `json:"mode"`
	At      time.Time         `json:"at"`
	Env     map[string]string `json:"environment"`
	Metrics []metric          `json:"metrics"`
}

func pass(b bool) *bool { return &b }

func main() {
	cfgPath := flag.String("config", "deploy/lite.json", "config file")
	runs := flag.Int("runs", 15, "repetitions for latency percentiles")
	outDir := flag.String("out", "bench/results", "results directory")
	md := flag.String("md", "docs/benchmarks.md", "markdown report (empty to skip)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	check(err)
	tmp, _ := os.MkdirTemp("", "fedsearch-bench-")
	defer os.RemoveAll(tmp)
	cfg.DataDir = tmp // cold start: discovery runs from scratch
	ctx := context.Background()
	mode := strings.TrimSuffix(filepath.Base(*cfgPath), filepath.Ext(*cfgPath))
	rep := &report{Mode: mode, At: time.Now().UTC(), Env: env()}
	add := func(m metric) {
		rep.Metrics = append(rep.Metrics, m)
		fmt.Printf("%-44s %-28s %s\n", m.Name, m.Value, verdict(m.Pass))
	}

	// 1. Catalog discovery cost: from scratch, then again with nothing changed.
	t0 := time.Now()
	fresh, full, err := catalog.Refresh(ctx, cfg, nil)
	check(err)
	coldStart := time.Since(t0)
	_, warm, err := catalog.Refresh(ctx, cfg, fresh)
	check(err)
	var reread int
	for _, l := range warm.Locations {
		reread += l.PartitionsRead
	}
	svc, err := service.New(ctx, cfg)
	check(err)
	for _, l := range full.Locations {
		if !strings.HasPrefix(l.Location, "cold.") || l.FilesRead == 0 {
			continue
		}
		per := l.MetadataBytes / int64(l.FilesRead)
		add(metric{Name: "footer bytes per Parquet file (" + l.Location + ")", Value: fmt.Sprintf("%s of %s avg file", bytes(per), bytes(l.FileBytes/int64(l.FilesRead))),
			Claim: "< 16 KB per file", Pass: pass(per < 16<<10),
			Detail: fmt.Sprintf("%.2f%% of bytes read to learn schema, row counts and row-group stats for %d files", 100*float64(l.MetadataBytes)/float64(l.FileBytes), l.FilesRead)})
	}
	add(metric{Name: "partitions re-read on unchanged refresh", Value: fmt.Sprint(reread), Claim: "0", Pass: pass(reread == 0)})
	add(metric{Name: "full discovery from scratch", Value: coldStart.Round(time.Millisecond).String(), Claim: "informational"})

	// Pin the clock so relative windows are reproducible against the data.
	gtNow := groundTruthNow()
	if !gtNow.IsZero() {
		now := gtNow.Add(10 * time.Hour)
		ir.Now = func() time.Time { return now }
		svc.Now = func() time.Time { return now }
	}
	run := func(q string, naive bool) *fexec.JobView {
		qq, err := svc.ParseQuery([]byte(q))
		check(err)
		p, err := svc.PlanWith(ctx, qq, naive)
		check(err)
		j, err := svc.Submit(ctx, p, service.SubmitOptions{Principal: "bench", Surface: "bench", AutoConfirm: true})
		check(err)
		<-j.Done()
		v := j.Snapshot()
		return &v
	}

	// 2. Pruning.
	cold := run(`{"dataset":"network_activity","time":{"from":"now-120d","to":"now-40d"},"group_by":["dst_endpoint.port"],"aggs":[{"fn":"sum","field":"traffic.bytes_out","as":"out"}]}`, false)
	s := cold.Plan.Slices[0]
	loc := svc.Catalog().Location(s.Location)
	act := cold.Result.Slices[0].Bytes
	add(metric{Name: "partition pruning (80-day window, cold)", Value: fmt.Sprintf("%d of %d partitions", len(s.Partitions), s.PartitionsTotal),
		Claim: "only partitions inside the window", Pass: pass(len(s.Partitions) <= 81)})
	var partBytes int64
	for _, p := range s.Partitions {
		partBytes += p.Bytes
	}
	add(metric{Name: "column pruning (3 of 11 columns)", Value: fmt.Sprintf("%s of %s", bytes(act), bytes(partBytes)),
		Claim: "reads only needed columns", Pass: pass(act < partBytes/2),
		Detail: fmt.Sprintf("%.1f%% of the selected partitions' bytes; location total %s", 100*float64(act)/float64(partBytes), bytes(loc.Bytes))})
	errPct := 100 * float64(s.Estimate.Bytes-act) / float64(max(act, 1))
	add(metric{Name: "cost estimate vs actual bytes (cold)", Value: fmt.Sprintf("est %s, actual %s (+%.1f%%)", bytes(s.Estimate.Bytes), bytes(act), errPct),
		Claim: "conservative, within 25%", Pass: pass(s.Estimate.Bytes >= act && errPct <= 25)})

	// 3. Latency per tier.
	hotQ := `{"dataset":"authentication","time":{"from":"now-7d"},"where":{"cmp":{"field":"status_id","op":"eq","value":2}},"group_by":["user.name"],"aggs":[{"fn":"count","as":"n"}]}`
	coldQ := `{"dataset":"authentication","time":{"from":"now-150d","to":"now-60d"},"where":{"cmp":{"field":"status_id","op":"eq","value":2}},"group_by":["user.name"],"aggs":[{"fn":"count","as":"n"}]}`
	bothQ := `{"dataset":"authentication","time":{"from":"now-180d"},"where":{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},"limit":500}`
	for _, c := range []struct{ name, q string }{{"hot tier only (7 days)", hotQ}, {"cold tier only (90 days)", coldQ}, {"both tiers (180 days, rows)", bothQ}} {
		var lat []time.Duration
		for i := 0; i < *runs; i++ {
			lat = append(lat, run(c.q, false).Result.Elapsed)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		add(metric{Name: "latency " + c.name, Value: fmt.Sprintf("p50 %s, p95 %s", ms(lat[len(lat)/2]), ms(lat[int(float64(len(lat))*0.95)])), Claim: "informational"})
	}

	// 4. Correctness against ground truth.
	failQ := `{"dataset":"authentication","time":{"from":"now-180d"},"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]},"aggs":[{"fn":"count","as":"n"}]}`
	exclusive := run(failQ, false).Result.Groups[0].Values["n"]
	naive := run(failQ, true).Result.Groups[0].Values["n"]
	add(metric{Name: "brute-force count across tiers", Value: fmt.Sprintf("%v (naive mode: %v)", exclusive, naive), Claim: "40 (ground truth)", Pass: pass(exclusive == int64(40))})
	naiveRows := run(strings.Replace(failQ, `"aggs":[{"fn":"count","as":"n"}]`, `"limit":500`, 1), true).Result
	add(metric{Name: "row dedup under naive overlap", Value: fmt.Sprintf("%d rows, %d duplicates dropped", len(naiveRows.Rows), naiveRows.Merge.Duplicates), Claim: "40 rows, 40 dropped",
		Pass: pass(len(naiveRows.Rows) == 40 && naiveRows.Merge.Duplicates == 40)})
	lat := run(`{"dataset":"authentication","time":{"from":"now-180d"},"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},{"cmp":{"field":"dst_endpoint.hostname","op":"eq","value":"db-prod-01"}}]},"enrich":["src_endpoint.ip"]}`, false).Result
	asOf, cur := "-", "-"
	if len(lat.Rows) == 1 {
		if c, ok := lat.Rows[0].Context["src_endpoint.ip"].(enrich.Context); ok && c.AsOf != nil && c.Current != nil {
			asOf, cur = c.AsOf.Hostname, c.Current.Hostname
		}
	}
	add(metric{Name: "lateral-movement attribution", Value: fmt.Sprintf("as-of %s, current %s", asOf, cur), Claim: "as-of = lt-ankit-042", Pass: pass(asOf == "lt-ankit-042")})

	// 5. NL accuracy on the golden set (needs an API key).
	if svc.NL.Available() {
		g := svc.NL.Golden
		svc.NL.Golden = nil // force the LLM; no cache fallback
		match, total := 0, 0
		var misses []string
		for _, e := range g.Entries {
			total++
			tr, err := svc.NL.Translate(ctx, e.Question)
			var want ir.Query
			_ = json.Unmarshal(e.IR, &want)
			if err == nil && svc.Prepare(&want) == nil && sameMeaning(tr.Query, &want) {
				match++
			} else {
				misses = append(misses, e.Question)
			}
		}
		svc.NL.Golden = g
		add(metric{Name: "NL -> IR on golden set", Value: fmt.Sprintf("%d / %d", match, total), Claim: ">= 90%", Pass: pass(float64(match) >= 0.9*float64(total)), Detail: strings.Join(misses, "; ")})
	} else {
		add(metric{Name: "NL -> IR on golden set", Value: "skipped", Claim: ">= 90%", Detail: "set ANTHROPIC_API_KEY to measure"})
	}

	check(os.MkdirAll(*outDir, 0o755))
	b, _ := json.MarshalIndent(rep, "", "  ")
	check(os.WriteFile(filepath.Join(*outDir, mode+".json"), b, 0o644))
	if *md != "" {
		check(os.WriteFile(*md, []byte(markdown(rep)), 0o644))
	}
	fmt.Printf("\nwrote %s and %s\n", filepath.Join(*outDir, mode+".json"), *md)
}

// sameMeaning compares what a query asks, ignoring presentation choices
// (select list, limit, order, aliases) that do not change the answer.
func sameMeaning(a, b *ir.Query) bool {
	key := func(q *ir.Query) string {
		var aggs []string
		for _, x := range q.Aggs {
			aggs = append(aggs, string(x.Fn)+":"+x.Field)
		}
		sort.Strings(aggs)
		c := *q
		c.Select, c.Order, c.Limit, c.Aggs, c.Having = nil, nil, 0, nil, nil
		h, _ := json.Marshal(struct {
			D, W string
			G    []string
			A    []string
			E    []string
			F, T int64
		}{c.Dataset, fmt.Sprint(canon(c.Where)), c.GroupBy, aggs, c.Enrich, c.Time.From.Unix() / 3600, c.Time.To.Unix() / 3600})
		return string(h)
	}
	return key(a) == key(b)
}

func canon(e *ir.Expr) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func groundTruthNow() time.Time {
	b, err := os.ReadFile("out/ground_truth.json")
	if err != nil {
		return time.Time{}
	}
	var gt struct {
		Now time.Time `json:"now"`
	}
	_ = json.Unmarshal(b, &gt)
	return gt.Now
}

func env() map[string]string {
	commit, _ := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	host, _ := os.Hostname()
	rows := "unknown"
	if b, err := os.ReadFile("out/ground_truth.json"); err == nil {
		var gt struct {
			Counts map[string]map[string]struct{ Events int } `json:"counts"`
		}
		_ = json.Unmarshal(b, &gt)
		var parts []string
		for ds, tiers := range gt.Counts {
			parts = append(parts, fmt.Sprintf("%s hot %d / cold %d", ds, tiers["hot"].Events, tiers["cold"].Events))
		}
		sort.Strings(parts)
		rows = strings.Join(parts, "; ")
	}
	return map[string]string{"commit": strings.TrimSpace(string(commit)), "go": runtime.Version(), "os_arch": runtime.GOOS + "/" + runtime.GOARCH,
		"cpus": fmt.Sprint(runtime.NumCPU()), "host": host, "data": rows}
}

func markdown(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmarks\n\nGenerated by `cmd/bench` (%s mode) on %s. Reproduce with `make bench`.\n\n", r.Mode, r.At.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "| Environment | |\n|---|---|\n")
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %s |\n", k, r.Env[k])
	}
	fmt.Fprintf(&b, "\n| Metric | Result | Claim | Status |\n|---|---|---|---|\n")
	for _, m := range r.Metrics {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", m.Name, m.Value, m.Claim, verdict(m.Pass))
	}
	b.WriteString("\n## Notes\n\n")
	for _, m := range r.Metrics {
		if m.Detail != "" {
			fmt.Fprintf(&b, "- **%s**: %s\n", m.Name, m.Detail)
		}
	}
	b.WriteString(`
- Lite mode runs the hot tier as NDJSON through DuckDB, so hot-tier "bytes scanned" are whole file sizes (a row format cannot skip columns). In Docker mode the hot tier is OpenSearch and its bytes are estimated as matched documents times the average document size.
- Cold-tier actual bytes are computed from Parquet footer metadata after row-group pruning on time, which is what DuckDB applies; the estimate prices whole partitions, so it is a deliberate upper bound.
`)
	return b.String()
}

func verdict(p *bool) string {
	if p == nil {
		return "–"
	}
	if *p {
		return "pass"
	}
	return "FAIL"
}

func bytes(n int64) string {
	f := float64(n)
	u := []string{"B", "KB", "MB", "GB"}
	i := 0
	for f >= 1000 && i < len(u)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func ms(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

var _ = nl.Normalize
