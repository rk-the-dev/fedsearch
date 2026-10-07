// Command demo walks through the five-act story in the terminal, in-process,
// with real queries against the configured stores. It is deterministic:
// natural-language steps use the golden set unless -llm is given.
//
//	go run ./cmd/demo                 # lite mode, pauses between acts
//	go run ./cmd/demo -pause=false    # run straight through (CI, rehearsal)
//	go run ./cmd/demo -config deploy/docker.json
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/enrich"
	"github.com/rksurwase/fedsearch/internal/investigate"
	"github.com/rksurwase/fedsearch/internal/service"
	"github.com/rksurwase/fedsearch/internal/tools"
)

var (
	pause = flag.Bool("pause", true, "wait for Enter between acts")
	in    = bufio.NewReader(os.Stdin)
)

const (
	bold  = "\033[1m"
	dim   = "\033[2m"
	red   = "\033[31m"
	amber = "\033[33m"
	blue  = "\033[34m"
	green = "\033[32m"
	reset = "\033[0m"
)

func act(n int, title, point string) {
	if *pause && n > 1 {
		fmt.Print(dim + "\n[Enter for the next act]" + reset)
		_, _ = in.ReadString('\n')
	}
	fmt.Printf("\n%s━━ Act %d: %s%s\n%s%s%s\n\n", bold, n, title, reset, dim, point, reset)
}

func main() {
	cfgPath := flag.String("config", "deploy/lite.json", "config file")
	useLLM := flag.Bool("llm", false, "translate questions with the LLM instead of the golden set")
	flag.Parse()
	cfg, err := config.Load(*cfgPath)
	check(err)
	if !*useLLM {
		cfg.LLM.Mode = "cache_only" // the demo must never depend on a network call
	}
	ctx := context.Background()
	svc, err := service.New(ctx, cfg)
	check(err)
	cat := svc.Catalog()

	act(1, "The problem", "One logical dataset, two physical homes, two query languages — and a window both tiers hold.")
	for _, ds := range []string{"authentication", "network_activity"} {
		fmt.Printf("%s%s%s\n", bold, ds, reset)
		for _, l := range cat.LocationsFor(ds) {
			color := blue
			if l.Tier == "hot" {
				color = amber
			}
			eff := l.Coverage.Effective(svc.Now())
			fmt.Printf("  %s%-5s%s %-26s %-11s %4d partitions  %s → %s\n", color, l.Tier, reset, l.ID, l.Caps.Dialect, len(l.Partitions),
				eff.From.Format("2006-01-02"), map[bool]string{true: "now", false: eff.To.Format("2006-01-02")}[l.Coverage.Live])
		}
		for _, o := range cat.Overlaps(ds) {
			fmt.Printf("  %soverlap%s %s → %s: every event in this window exists twice\n", red, reset, o.Range.From.Format("2006-01-02"), o.Range.To.Format("2006-01-02"))
		}
	}

	act(2, "Ask once", "Plain language becomes typed IR; the planner slices time across tiers, prunes partitions, and prices it before anything runs.")
	q1 := "How many failed logins did svc_backup have in the last 6 months?"
	fmt.Printf("%s> %s%s\n", bold, q1, reset)
	tr, err := svc.Ask(ctx, q1, "demo", "demo")
	check(err)
	b, _ := json.Marshal(tr.Query)
	fmt.Printf("%sIR (%s):%s %s\n\n", dim, tr.Source, reset, b)
	plan, err := svc.Plan(ctx, tr.Query)
	check(err)
	for _, s := range plan.Slices {
		fmt.Printf("  %s %-24s %s → %s  %d/%d partitions  est %s  $%.6f\n", s.ID, s.Location, s.Time.From.Format("2006-01-02"), s.Time.To.Format("2006-01-02 15:04"),
			len(s.Partitions), s.PartitionsTotal, human(s.Estimate.Bytes), s.Estimate.USD)
	}
	fmt.Printf("  %stotal estimate $%.6f, decided before execution%s\n", bold, plan.Total.USD, reset)
	fmt.Printf("\n%sDuckDB SQL for %s (note status = 'failure': the OCSF enum was mapped to the stored caption):%s\n%s\n",
		dim, plan.Slices[0].ID, reset, indent(plan.Slices[0].Native))

	act(3, "Correct answers", "The overlap window is owned by one tier. Turn that off and the count doubles.")
	j, err := svc.Submit(ctx, plan, service.SubmitOptions{Principal: "demo", Surface: "demo", AutoConfirm: true})
	check(err)
	<-j.Done()
	r := j.Snapshot().Result
	fmt.Printf("  correct:  %s%v failed logins%s  (%s, %s scanned)\n", green+bold, r.Groups[0].Values["failures"], reset, r.Elapsed.Round(time.Millisecond), human(r.Bytes))
	naive, err := svc.PlanWith(ctx, tr.Query, true)
	check(err)
	jn, err := svc.Submit(ctx, naive, service.SubmitOptions{Principal: "demo", Surface: "demo", AutoConfirm: true})
	check(err)
	<-jn.Done()
	fmt.Printf("  naive:    %s%v failed logins%s  (both tiers answered the overlap window)\n", red+bold, jn.Snapshot().Result.Groups[0].Values["failures"], reset)
	fmt.Printf("  %sRow queries survive overlap by dedup on event identity; aggregates cannot, so disjoint slices are a correctness rule.%s\n", dim, reset)

	act(4, "Context", "Who was 10.20.4.17 when svc_backup was used against the production database?")
	q2 := "Where did svc_backup log in from in the last 6 months?"
	fmt.Printf("%s> %s%s\n", bold, q2, reset)
	tr2, err := svc.Ask(ctx, q2, "demo", "demo")
	check(err)
	j2, err := svc.Run(ctx, tr2.Query, service.SubmitOptions{Principal: "demo", Surface: "demo", AutoConfirm: true})
	check(err)
	for _, row := range j2.Snapshot().Result.Rows {
		c, ok := row.Context["src_endpoint.ip"].(enrich.Context)
		if !ok || c.AsOf == nil {
			continue
		}
		fmt.Printf("  %s  %s → %s from %s\n", row.Time.Format("2006-01-02 15:04"), row.Fields["user.name"], row.Fields["dst_endpoint.hostname"], row.Fields["src_endpoint.ip"])
		fmt.Printf("     as of event time: %s%s (%s)%s\n", green+bold, c.AsOf.Hostname, c.AsOf.Owner, reset)
		fmt.Printf("     current owner:    %s%s (%s)%s  ← a naive join blames this laptop\n", red, c.Current.Hostname, c.Current.Owner, reset)
	}

	act(5, "Agents, safely", "One indicator in; a cited timeline out — through governed, audited, budgeted tools. Log data is never instructions.")
	hub := tools.NewHub(svc, "demo")
	hub.MaxRows = 2000
	rep, err := investigate.Playbook(ctx, hub, tools.PrincipalFrom(cfg.Agents[0]), "185.220.101.47", "now-180d")
	check(err)
	for _, e := range rep.Timeline {
		fmt.Printf("  %s  %s%-18s%s %s\n", e.Start.Format("2006-01-02 15:04"), bold, e.Stage, reset, e.Summary)
		var cs []string
		for _, c := range e.Citations {
			cs = append(cs, c.Location+"/"+c.EventID)
		}
		fmt.Printf("  %s%17s cites %s%s\n", dim, "", strings.Join(cs, ", "), reset)
	}
	fmt.Println()
	for _, f := range rep.Findings {
		fmt.Printf("  %s•%s %s\n", red, reset, f)
	}
	recs, _ := svc.Audit.Recent(1000)
	fmt.Printf("\n%s%d governed queries, $%.5f, %s. %d audit records written.%s\n", dim, len(rep.Steps), rep.USD, rep.Elapsed.Round(time.Millisecond), len(recs), reset)
	fmt.Printf("\n%sOpen the console for the same story with the visuals: go run ./cmd/server -config %s%s\n", bold, *cfgPath, reset)
}

func human(n int64) string {
	f := float64(n)
	u := []string{"B", "KB", "MB", "GB"}
	i := 0
	for f >= 1000 && i < len(u)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if len(l) > 160 {
			l = l[:160] + "…"
		}
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}
