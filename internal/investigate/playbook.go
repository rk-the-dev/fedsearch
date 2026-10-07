// Package investigate builds an attack timeline from one indicator (the
// Lumen stand-in). The playbook is deterministic: it pivots through the same
// governed tools an AI agent gets (tools.Hub), so every step is audited,
// budgeted and cited. An optional LLM agent mode (agent.go) drives the same
// tools with Claude.
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/enrich"
	"github.com/rksurwase/fedsearch/internal/tools"
)

type Step struct {
	Title    string          `json:"title"`
	Tool     string          `json:"tool"`
	Query    json.RawMessage `json:"query"`
	Job      string          `json:"job"`
	Rows     int             `json:"rows"`
	Tiers    []string        `json:"tiers"`
	USD      float64         `json:"usd"`
	Summary  string          `json:"summary"`
	Duration time.Duration   `json:"duration_ns"`
}

type Entry struct {
	Start     time.Time        `json:"start"`
	End       time.Time        `json:"end"`
	Stage     string           `json:"stage"`
	Summary   string           `json:"summary"`
	Severity  string           `json:"severity"`
	Citations []tools.Citation `json:"citations"`
	Evidence  int              `json:"evidence_events"`
}

type Attribution struct {
	IP        string    `json:"ip"`
	At        time.Time `json:"at"`
	AsOfHost  string    `json:"as_of_host"`
	AsOfOwner string    `json:"as_of_owner"`
	NowHost   string    `json:"current_host"`
	NowOwner  string    `json:"current_owner"`
	Changed   bool      `json:"changed"`
}

type Report struct {
	Indicator    string        `json:"indicator"`
	Window       string        `json:"window"`
	Mode         string        `json:"mode"` // playbook | agent
	Steps        []Step        `json:"steps"`
	Timeline     []Entry       `json:"timeline"`
	Findings     []string      `json:"findings"`
	Attributions []Attribution `json:"attributions"`
	Flagged      int           `json:"flagged_values"`
	USD          float64       `json:"usd"`
	Narrative    string        `json:"narrative,omitempty"` // agent mode
	Elapsed      time.Duration `json:"elapsed_ns"`
}

type runner struct {
	ctx context.Context
	hub *tools.Hub
	p   tools.Principal
	rep *Report
}

func (r *runner) search(title string, q map[string]any) (tools.RunOutput, error) {
	raw, _ := json.Marshal(map[string]any{"query": q})
	t0 := time.Now()
	out, err := r.hub.Call(r.ctx, r.p, "search_run", raw)
	if err != nil {
		return tools.RunOutput{}, fmt.Errorf("%s: %w", title, err)
	}
	res := out.(tools.RunOutput)
	qraw, _ := json.Marshal(q)
	r.rep.Steps = append(r.rep.Steps, Step{Title: title, Tool: "search_run", Query: qraw, Job: res.Job, Rows: res.TotalRows,
		Tiers: res.Tiers, USD: res.USD, Duration: time.Since(t0)})
	r.rep.Flagged += res.Flagged
	r.rep.USD += res.USD
	return res, nil
}

func (r *runner) note(s string) { r.rep.Steps[len(r.rep.Steps)-1].Summary = s }

func cmp(field, op string, v any) map[string]any {
	return map[string]any{"cmp": map[string]any{"field": field, "op": op, "value": v}}
}

func str(v any) string { return fmt.Sprint(v) }

// Playbook investigates one external indicator IP over a time window.
func Playbook(ctx context.Context, hub *tools.Hub, p tools.Principal, ioc, window string) (*Report, error) {
	start := time.Now()
	if window == "" {
		window = "now-180d"
	}
	r := &runner{ctx: ctx, hub: hub, p: p, rep: &Report{Indicator: ioc, Window: window, Mode: "playbook"}}
	tw := map[string]any{"from": window, "to": "now"}

	// 1. Inbound activity from the indicator.
	inbound, err := r.search("Inbound connections from the indicator", map[string]any{
		"dataset": "network_activity", "time": tw, "where": cmp("src_endpoint.ip", "eq", ioc), "limit": 2000})
	if err != nil {
		return nil, err
	}
	if n := len(inbound.Rows); n > 0 {
		blocked, ports, targets := 0, map[string]int{}, map[string]bool{}
		for _, row := range allRows(inbound) {
			if row.Fields["action_id"] == int64(2) {
				blocked++
			}
			ports[str(row.Fields["dst_endpoint.port"])]++
			targets[str(row.Fields["dst_endpoint.ip"])] = true
		}
		rows := allRows(inbound)
		e := Entry{Start: rows[0].Time, End: rows[len(rows)-1].Time, Stage: "Reconnaissance", Severity: "medium", Evidence: inbound.TotalRows,
			Summary: fmt.Sprintf("%d connection attempts from %s to %d internal hosts on ports %s; %d blocked",
				inbound.TotalRows, ioc, len(targets), topKeys(ports, 5), blocked),
			Citations: cites(rows, 2)}
		r.rep.Timeline = append(r.rep.Timeline, e)
		r.note(e.Summary)
	} else {
		r.note("no inbound network activity")
	}

	// 2. Authentication attempts from the indicator.
	auths, err := r.search("Authentication attempts from the indicator", map[string]any{
		"dataset": "authentication", "time": tw, "where": cmp("src_endpoint.ip", "eq", ioc), "limit": 2000})
	if err != nil {
		return nil, err
	}
	compromised := map[string]time.Time{}
	if rows := allRows(auths); len(rows) > 0 {
		fails := map[string]int{}
		var failRows, okRows []tools.AgentRow
		for _, row := range rows {
			u := str(row.Fields["user.name"])
			if row.Fields["status_id"] == int64(2) {
				fails[u]++
				failRows = append(failRows, row)
			} else if row.Fields["status_id"] == int64(1) {
				okRows = append(okRows, row)
				if _, seen := compromised[u]; !seen {
					compromised[u] = row.Time
				}
			}
		}
		e := Entry{Start: rows[0].Time, End: rows[len(rows)-1].Time, Stage: "Credential access", Severity: "high", Evidence: len(rows),
			Summary:   fmt.Sprintf("%d failed logons (%s) from %s", len(failRows), topKeys(fails, 3), ioc),
			Citations: cites(failRows, 2)}
		if len(okRows) > 0 {
			var users []string
			for u := range compromised {
				users = append(users, u)
			}
			sort.Strings(users)
			e.Summary += fmt.Sprintf(", then %d successful logon(s) as %s to %s", len(okRows), strings.Join(users, ", "), str(okRows[0].Fields["dst_endpoint.hostname"]))
			e.Severity = "critical"
			e.Citations = append(e.Citations, cites(okRows, 1)...)
		}
		r.rep.Timeline = append(r.rep.Timeline, e)
		r.note(e.Summary)
	} else {
		r.note("no authentication attempts")
	}

	// 3. Lateral movement: compromised accounts used from internal hosts.
	for user, since := range compromised {
		lat, err := r.search("Use of "+user+" from other sources", map[string]any{
			"dataset": "authentication", "time": map[string]any{"from": since.Format(time.RFC3339), "to": "now"},
			"where":  map[string]any{"and": []any{cmp("user.name", "eq", user), cmp("status_id", "eq", 1), map[string]any{"not": cmp("src_endpoint.ip", "eq", ioc)}}},
			"enrich": []string{"src_endpoint.ip"}, "limit": 500})
		if err != nil {
			return nil, err
		}
		for _, row := range allRows(lat) {
			a := attribution(row, "src_endpoint.ip")
			if a.IP != "" {
				r.rep.Attributions = append(r.rep.Attributions, a)
			}
			e := Entry{Start: row.Time, End: row.Time, Stage: "Lateral movement", Severity: "critical", Evidence: 1,
				Summary: fmt.Sprintf("%s used from %s (%s, owner %s, as of event time) against %s",
					user, a.IP, a.AsOfHost, a.AsOfOwner, str(row.Fields["dst_endpoint.hostname"])),
				Citations: []tools.Citation{row.Cite}}
			if a.Changed {
				e.Summary += fmt.Sprintf("; today %s belongs to %s — a current-owner join would blame the wrong device", a.IP, a.NowHost)
			}
			r.rep.Timeline = append(r.rep.Timeline, e)

			// Data pulled from the target by that host after the logon.
			pull, err := r.search("Traffic from "+a.IP+" after the logon", map[string]any{
				"dataset": "network_activity", "time": map[string]any{"from": row.Time.Format(time.RFC3339), "to": row.Time.Add(6 * time.Hour).Format(time.RFC3339)},
				"where":    cmp("src_endpoint.ip", "eq", a.IP),
				"group_by": []string{"dst_endpoint.ip", "dst_endpoint.port"},
				"aggs":     []any{map[string]any{"fn": "sum", "field": "traffic.bytes_in", "as": "bytes_in"}, map[string]any{"fn": "count", "as": "flows"}},
				"order":    map[string]any{"by": "bytes_in", "desc": true}, "limit": 3})
			if err != nil {
				return nil, err
			}
			if len(pull.Groups) > 0 {
				g := pull.Groups[0].Values
				r.note(fmt.Sprintf("%s pulled %s from %s:%v in %v flows", a.IP, human(g["bytes_in"]), str(g["dst_endpoint.ip"]), g["dst_endpoint.port"], g["flows"]))
				e2 := Entry{Start: row.Time, End: row.Time.Add(15 * time.Minute), Stage: "Collection", Severity: "high", Evidence: toInt(g["flows"]),
					Summary:   fmt.Sprintf("%s (%s) received %s from %s port %v", a.IP, a.AsOfHost, human(g["bytes_in"]), str(g["dst_endpoint.ip"]), g["dst_endpoint.port"]),
					Citations: []tools.Citation{{Job: pull.Job, Location: "aggregate", EventID: fmt.Sprintf("group %s:%v", str(g["dst_endpoint.ip"]), g["dst_endpoint.port"])}}}
				r.rep.Timeline = append(r.rep.Timeline, e2)
			}
		}
	}

	// 4. Outbound transfers to the indicator.
	out, err := r.search("Outbound transfers to the indicator", map[string]any{
		"dataset": "network_activity", "time": tw, "where": cmp("dst_endpoint.ip", "eq", ioc), "enrich": []string{"src_endpoint.ip"}, "limit": 2000})
	if err != nil {
		return nil, err
	}
	if rows := allRows(out); len(rows) > 0 {
		var total int64
		bySrc := map[string]Attribution{}
		for _, row := range rows {
			total += toInt64(row.Fields["traffic.bytes_out"])
			if a := attribution(row, "src_endpoint.ip"); a.IP != "" {
				bySrc[a.IP] = a
			}
		}
		var srcs []string
		for ip, a := range bySrc {
			srcs = append(srcs, fmt.Sprintf("%s (%s, owner %s)", ip, a.AsOfHost, a.AsOfOwner))
			r.rep.Attributions = append(r.rep.Attributions, a)
		}
		sort.Strings(srcs)
		e := Entry{Start: rows[0].Time, End: rows[len(rows)-1].Time, Stage: "Exfiltration", Severity: "critical", Evidence: len(rows),
			Summary:   fmt.Sprintf("%s sent to %s in %d flows from %s", human(total), ioc, len(rows), strings.Join(srcs, ", ")),
			Citations: cites(rows, 3)}
		r.rep.Timeline = append(r.rep.Timeline, e)
		r.note(e.Summary)
	} else {
		r.note("no outbound transfers")
	}

	sort.SliceStable(r.rep.Timeline, func(i, j int) bool { return r.rep.Timeline[i].Start.Before(r.rep.Timeline[j].Start) })
	r.findings()
	r.rep.Elapsed = time.Since(start)
	return r.rep, nil
}

func (r *runner) findings() {
	hosts := map[string]string{}
	for _, a := range r.rep.Attributions {
		if a.AsOfHost != "" {
			hosts[a.AsOfHost] = a.AsOfOwner
		}
	}
	for _, e := range r.rep.Timeline {
		if e.Stage == "Credential access" && e.Severity == "critical" {
			r.rep.Findings = append(r.rep.Findings, "Account compromise: "+e.Summary)
		}
	}
	var hs []string
	for h, o := range hosts {
		hs = append(hs, fmt.Sprintf("%s (owner %s)", h, o))
	}
	sort.Strings(hs)
	if len(hs) > 0 {
		r.rep.Findings = append(r.rep.Findings, "Compromised host(s), attributed as of event time: "+strings.Join(hs, ", "))
	}
	for _, a := range r.rep.Attributions {
		if a.Changed {
			r.rep.Findings = append(r.rep.Findings, fmt.Sprintf("Attribution trap avoided: %s belonged to %s at %s but to %s today.",
				a.IP, a.AsOfHost, a.At.Format("2006-01-02 15:04"), a.NowHost))
		}
	}
	for _, e := range r.rep.Timeline {
		if e.Stage == "Exfiltration" {
			r.rep.Findings = append(r.rep.Findings, "Data loss: "+e.Summary)
		}
	}
	if r.rep.Flagged > 0 {
		r.rep.Findings = append(r.rep.Findings, fmt.Sprintf("%d log value(s) contained prompt-injection text addressed to an AI analyst; withheld from the agent and ignored.", r.rep.Flagged))
	}
}

func allRows(o tools.RunOutput) []tools.AgentRow { return o.Rows }

func cites(rows []tools.AgentRow, n int) []tools.Citation {
	var out []tools.Citation
	for i := 0; i < len(rows) && i < n; i++ {
		out = append(out, rows[i].Cite)
	}
	return out
}

func attribution(row tools.AgentRow, field string) Attribution {
	a := Attribution{IP: str(row.Fields[field]), At: row.Time}
	c, ok := row.Context[field].(enrich.Context)
	if !ok {
		return Attribution{}
	}
	if c.AsOf != nil {
		a.AsOfHost, a.AsOfOwner = c.AsOf.Hostname, c.AsOf.Owner
	}
	if c.Current != nil {
		a.NowHost, a.NowOwner = c.Current.Hostname, c.Current.Owner
	}
	a.Changed = c.Changed
	return a
}

func topKeys(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v || (l[i].v == l[j].v && l[i].k < l[j].k) })
	var parts []string
	for i := 0; i < len(l) && i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s ×%d", l[i].k, l[i].v))
	}
	return strings.Join(parts, ", ")
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func toInt(v any) int { return int(toInt64(v)) }

func human(v any) string {
	b := float64(toInt64(v))
	if f, ok := v.(float64); ok {
		b = f
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for b >= 1000 && i < len(units)-1 {
		b /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}
