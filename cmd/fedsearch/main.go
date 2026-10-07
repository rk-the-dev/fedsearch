// Command fedsearch is the CLI: catalog maintenance, questions, IR queries
// and investigations, against the same service the server runs.
//
//	fedsearch catalog refresh
//	fedsearch catalog show
//	fedsearch ask "Show failed logins for svc_backup in the last 6 months"
//	fedsearch query '{"dataset":"network_activity","time":{"from":"now-7d"},"aggs":[{"fn":"count","as":"n"}]}'
//	fedsearch plan  '<IR>'
//	fedsearch investigate 185.220.101.47
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/exec"
	"github.com/rksurwase/fedsearch/internal/investigate"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/service"
	"github.com/rksurwase/fedsearch/internal/tools"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: fedsearch [-config file] [-json] [-naive] <command> [args]

commands:
  catalog refresh        rediscover every location (footer-only for Parquet)
  catalog show           datasets, locations, coverage and overlaps
  ask "<question>"       natural language -> IR -> plan -> run
  plan '<IR json>'       show the plan and cost without running
  query '<IR json>'      plan and run an IR query
  investigate <ip>       run the investigation playbook from an indicator`)
	os.Exit(2)
}

func main() {
	cfgPath := flag.String("config", "deploy/lite.json", "config file")
	asJSON := flag.Bool("json", false, "print JSON")
	naive := flag.Bool("naive", false, "disable overlap ownership (demonstrates double counting)")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}
	cfg, err := config.Load(*cfgPath)
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	svc, err := service.New(ctx, cfg)
	check(err)
	out := printer{json: *asJSON}

	switch args[0] {
	case "catalog":
		if len(args) < 2 {
			usage()
		}
		switch args[1] {
		case "refresh":
			rep, err := svc.Refresh(ctx)
			check(err)
			if out.json {
				out.print(rep)
				return
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', tabwriter.AlignRight)
			fmt.Fprintln(tw, "location\tpartitions\tread\tmetadata bytes\tfile bytes\tratio\t")
			for _, l := range rep.Locations {
				ratio := "-"
				if l.FileBytes > 0 {
					ratio = fmt.Sprintf("%.2f%%", 100*float64(l.MetadataBytes)/float64(l.FileBytes))
				}
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t\n", l.Location, l.Partitions, l.PartitionsRead, l.MetadataBytes, l.FileBytes, ratio)
			}
			tw.Flush()
			for _, o := range rep.Overlaps {
				fmt.Printf("overlap %s: %s and %s both hold %s\n", o.Dataset, o.A, o.B, o.Range)
			}
			for _, e := range rep.Errors {
				fmt.Println("error:", e)
			}
		case "show":
			cat := svc.Catalog()
			if out.json {
				out.print(cat)
				return
			}
			var names []string
			for n := range cat.Datasets {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				d := cat.Datasets[n]
				fmt.Printf("%s (OCSF %s %d), %d fields\n", n, d.ClassName, d.ClassUID, len(d.Fields))
				for _, l := range cat.LocationsFor(n) {
					fmt.Printf("  %-26s %-10s %4d partitions %10d rows  %s\n", l.ID, l.Caps.Engine, len(l.Partitions), l.Rows, l.Coverage.Effective(svc.Now()))
				}
				for _, o := range cat.Overlaps(n) {
					fmt.Printf("  overlap: %s (%.0f days)\n", o.Range, o.Range.Duration().Hours()/24)
				}
			}
		default:
			usage()
		}
	case "ask":
		need(args, 2)
		tr, err := svc.Ask(ctx, strings.Join(args[1:], " "), "cli", "cli")
		check(err)
		fmt.Printf("IR (%s): %s\n", tr.Source, tr.Explanation)
		b, _ := json.Marshal(tr.Query)
		fmt.Println(string(b))
		runQuery(ctx, svc, tr.Query, *naive, out)
	case "plan", "query":
		need(args, 2)
		q, err := svc.ParseQuery([]byte(args[1]))
		check(err)
		if args[0] == "plan" {
			p, err := svc.PlanWith(ctx, q, *naive)
			check(err)
			if out.json {
				out.print(p)
				return
			}
			printPlan(p)
			return
		}
		runQuery(ctx, svc, q, *naive, out)
	case "investigate":
		need(args, 2)
		if len(cfg.Agents) == 0 {
			check(fmt.Errorf("no agent principal configured"))
		}
		hub := tools.NewHub(svc, "cli")
		hub.MaxRows = 2000
		rep, err := investigate.Playbook(ctx, hub, tools.PrincipalFrom(cfg.Agents[0]), args[1], "now-180d")
		check(err)
		if out.json {
			out.print(rep)
			return
		}
		for _, e := range rep.Timeline {
			fmt.Printf("%s  %-18s %s\n", e.Start.Format("2006-01-02 15:04"), e.Stage, e.Summary)
		}
		fmt.Println()
		for _, f := range rep.Findings {
			fmt.Println("finding:", f)
		}
		fmt.Printf("\n%d governed queries, $%.5f, %s\n", len(rep.Steps), rep.USD, rep.Elapsed.Round(time.Millisecond))
	default:
		usage()
	}
}

func runQuery(ctx context.Context, svc *service.Service, q *ir.Query, naive bool, out printer) {
	p, err := svc.PlanWith(ctx, q, naive)
	check(err)
	if !out.json {
		printPlan(p)
	}
	j, err := svc.Submit(ctx, p, service.SubmitOptions{Principal: "cli", Surface: "cli", AutoConfirm: true})
	check(err)
	<-j.Done()
	snap := j.Snapshot()
	if out.json {
		out.print(snap.Result)
		return
	}
	printResult(q, snap.Result)
}

func printPlan(p *planner.Plan) {
	fmt.Printf("plan %s: %d slice(s), est. %d bytes, $%.6f\n", p.ID, len(p.Slices), p.Total.Bytes, p.Total.USD)
	for _, s := range p.Slices {
		fmt.Printf("  %s %-24s %s  %d/%d partitions  est %d B  residual=%v\n", s.ID, s.Location, s.Time, len(s.Partitions), s.PartitionsTotal, s.Estimate.Bytes, s.Residual != nil)
	}
	for _, w := range p.Warnings {
		fmt.Println("  warning:", w)
	}
}

func printResult(q *ir.Query, r *exec.Result) {
	fmt.Printf("%s in %s: %d bytes scanned, $%.6f, %d duplicates removed, %d flagged\n",
		r.Status, r.Elapsed.Round(time.Millisecond), r.Bytes, r.USD, r.Merge.Duplicates, r.Flagged)
	if r.Reason != "" {
		fmt.Println("  reason:", r.Reason)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if q.IsAggregate() {
		cols := append(append([]string{}, q.GroupBy...), aliases(q)...)
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
		for _, g := range r.Groups {
			var vals []string
			for _, c := range cols {
				vals = append(vals, fmt.Sprint(g.Values[c]))
			}
			fmt.Fprintln(tw, strings.Join(vals, "\t"))
		}
	} else {
		cols := append([]string{"time"}, q.Select...)
		fmt.Fprintln(tw, strings.Join(cols, "\t")+"\tlocation\tcontext")
		for i, row := range r.Rows {
			if i >= 50 {
				fmt.Fprintf(tw, "… %d more rows\n", len(r.Rows)-50)
				break
			}
			vals := []string{row.Time.Format("2006-01-02 15:04:05")}
			for _, c := range q.Select {
				if c == "time" {
					continue
				}
				vals = append(vals, truncate(fmt.Sprint(row.Fields[c]), 40))
			}
			ctxs := ""
			for f, c := range row.Context {
				b, _ := json.Marshal(c)
				ctxs += f + "=" + truncate(string(b), 120) + " "
			}
			fmt.Fprintln(tw, strings.Join(vals, "\t")+"\t"+row.Location+"\t"+ctxs)
		}
	}
	tw.Flush()
}

func aliases(q *ir.Query) []string {
	var out []string
	for _, a := range q.Aggs {
		out = append(out, a.As)
	}
	return out
}

type printer struct{ json bool }

func (p printer) print(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func need(args []string, n int) {
	if len(args) < n {
		usage()
	}
}

func check(err error) {
	if err != nil {
		var ve *ir.ValidationError
		if e, ok := err.(*ir.ValidationError); ok {
			ve = e
			fmt.Fprintln(os.Stderr, "invalid query:")
			for _, p := range ve.Problems {
				fmt.Fprintln(os.Stderr, "  -", p)
			}
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
