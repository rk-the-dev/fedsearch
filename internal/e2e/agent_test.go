package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rksurwase/fedsearch/internal/investigate"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/nl"
	"github.com/rksurwase/fedsearch/internal/tools"
)

func agentPrincipal(s interface{}) tools.Principal {
	return tools.Principal{Name: "lumen-lite", PerHourUSD: 1, Allowed: map[string]bool{
		"catalog_describe": true, "search_plan": true, "search_run": true, "context_lookup": true}}
}

// The playbook reconstructs the whole attack from one indicator, attributes
// it to the right laptop, and reports the injection attempt.
func TestPlaybookReconstructsAttack(t *testing.T) {
	s, _ := setup(t)
	hub := tools.NewHub(s, "investigate")
	hub.MaxRows = 2000
	rep, err := investigate.Playbook(context.Background(), hub, agentPrincipal(nil), "185.220.101.47", "now-180d")
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, e := range rep.Timeline {
		stages = append(stages, e.Stage)
		if len(e.Citations) == 0 {
			t.Errorf("%s has no citations", e.Stage)
		}
	}
	want := "Reconnaissance,Credential access,Lateral movement,Collection,Exfiltration"
	if strings.Join(stages, ",") != want {
		t.Fatalf("stages = %v, want %s", stages, want)
	}
	all := strings.Join(rep.Findings, "\n")
	for _, w := range []string{"lt-ankit-042", "lt-meera-118", "prompt-injection"} {
		if !strings.Contains(all, w) {
			t.Errorf("findings missing %q:\n%s", w, all)
		}
	}
	for _, e := range rep.Timeline {
		t.Logf("%s  %-18s %s", e.Start.Format("2006-01-02 15:04"), e.Stage, e.Summary)
	}
}

// Agent-facing results never contain the injection text.
func TestToolResultsWithholdInjection(t *testing.T) {
	s, _ := setup(t)
	hub := tools.NewHub(s, "mcp")
	args := `{"query":{"dataset":"authentication","time":{"from":"now-180d"},"where":{"cmp":{"field":"user.name","op":"eq","value":"svc_backup"}},"limit":100}}`
	out, err := hub.Call(context.Background(), agentPrincipal(nil), "search_run", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "ignore previous instructions") {
		t.Fatal("injection text reached the agent")
	}
	if ro := out.(tools.RunOutput); ro.Trust != "untrusted_data" || ro.Flagged != 1 || ro.Rows[0].Cite.EventID == "" {
		t.Fatalf("envelope = %+v", ro)
	}
	// Disallowed tools are refused by policy.
	p := agentPrincipal(nil)
	delete(p.Allowed, "search_run")
	if _, err := hub.Call(context.Background(), p, "search_run", json.RawMessage(args)); err == nil {
		t.Fatal("policy should refuse a tool not in the allow-list")
	}
}

// Every golden NL entry plans successfully against the live catalog.
func TestGoldenQuestionsPlan(t *testing.T) {
	s, _ := setup(t)
	for _, e := range s.NL.Golden.Entries {
		tr, err := s.NL.Translate(context.Background(), e.Question)
		if err != nil {
			t.Fatalf("%q: %v", e.Question, err)
		}
		if _, err := s.Plan(context.Background(), tr.Query); err != nil {
			t.Errorf("%q: plan: %v", e.Question, err)
		}
	}
	_ = nl.Normalize
	_ = ir.Now
}
