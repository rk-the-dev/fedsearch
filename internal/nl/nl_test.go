package nl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/ir"
)

func testCatalog() *catalog.Catalog {
	return &catalog.Catalog{Datasets: catalog.NewDatasets()}
}

func prepare(q *ir.Query) error {
	c := testCatalog()
	ir.Normalize(q, c)
	return ir.Validate(q, c)
}

// scripted LLM: returns canned tool inputs in order and records conversations.
type scripted struct {
	outputs []string
	convs   [][]Message
}

func (s *scripted) EmitQuery(_ context.Context, _ string, conv []Message) (*ToolCall, error) {
	s.convs = append(s.convs, conv)
	i := len(s.convs) - 1
	if i >= len(s.outputs) {
		return nil, fmt.Errorf("no more outputs")
	}
	return &ToolCall{ID: fmt.Sprintf("tu_%d", i), Input: json.RawMessage(s.outputs[i])}, nil
}

func TestGoldenSetIsValid(t *testing.T) {
	g, err := LoadGolden("../../testdata/nl/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entries) < 15 {
		t.Fatalf("golden set has %d entries", len(g.Entries))
	}
	for _, e := range g.Entries {
		var q ir.Query
		if err := json.Unmarshal(e.IR, &q); err != nil {
			t.Fatalf("%q: %v", e.Question, err)
		}
		if err := prepare(&q); err != nil {
			t.Errorf("%q: %v", e.Question, err)
		}
	}
	if _, ok := g.Lookup("  failed LOGINS for svc_backup?? "); !ok {
		t.Error("alias lookup should ignore case and punctuation")
	}
}

func TestRepairLoop(t *testing.T) {
	llm := &scripted{outputs: []string{
		`{"explanation":"x","query":{"dataset":"authentication","time":{"from":"now-7d"},"where":{"cmp":{"field":"status","op":"eq","value":"failure"}}}}`,
		`{"explanation":"failed logins","query":{"dataset":"authentication","time":{"from":"now-7d"},"where":{"cmp":{"field":"status_id","op":"eq","value":2}}}}`,
	}}
	tr := &Translator{Cfg: config.LLM{Mode: "llm_first"}, LLM: llm, Catalog: testCatalog, Prepare: prepare}
	out, err := tr.Translate(context.Background(), "failed logins this week")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Attempts) != 2 || len(out.Attempts[0].Problems) == 0 || out.Source != "llm" {
		t.Fatalf("attempts = %+v", out.Attempts)
	}
	repair := llm.convs[1][len(llm.convs[1])-1].Content[0]
	if repair.Type != "tool_result" || !repair.IsError || !strings.Contains(repair.Content, "unknown field") {
		t.Fatalf("repair turn = %+v", repair)
	}
}

func TestFallbackToGoldenWhenLLMFails(t *testing.T) {
	g, _ := LoadGolden("../../testdata/nl/golden.json")
	tr := &Translator{Cfg: config.LLM{Mode: "llm_first"}, LLM: &scripted{}, Golden: g, Catalog: testCatalog, Prepare: prepare}
	out, err := tr.Translate(context.Background(), "Show failed logins for svc_backup in the last 6 months")
	if err != nil || out.Source != "cache" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if _, err := tr.Translate(context.Background(), "something nobody asked before"); err == nil {
		t.Fatal("expected error for unknown question with failing LLM")
	}
}

func TestPromptFencesCatalogAndOmitsSuspicious(t *testing.T) {
	c := testCatalog()
	f := c.FieldDef("authentication", "http_request.user_agent")
	f.Stats = &catalog.FieldStats{Examples: []catalog.Sample{{Value: "[withheld]", Suspicious: true}}}
	p := SystemPrompt(c, ir.Now())
	if !strings.Contains(p, "<catalog_data>") || !strings.Contains(p, "status_id (int)") || !strings.Contains(p, "2=Failure") {
		t.Fatal("prompt missing grounding")
	}
	if strings.Contains(p, "withheld") {
		t.Fatal("suspicious samples must never reach the prompt")
	}
}

type broke struct{ calls int }

func (b *broke) EmitQuery(context.Context, string, []Message) (*ToolCall, error) {
	b.calls++
	return nil, ParseAPIError(400, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`))
}

// A permanent API error (no credits, bad key) switches the LLM off once;
// later questions go straight to the golden set without calling the API.
func TestPermanentLLMErrorDisablesLLM(t *testing.T) {
	g, _ := LoadGolden("../../testdata/nl/golden.json")
	llm := &broke{}
	tr := &Translator{Cfg: config.LLM{Mode: "llm_first"}, LLM: llm, Golden: g, Catalog: testCatalog, Prepare: prepare}
	out, err := tr.Translate(context.Background(), "Show failed logins for svc_backup in the last 6 months")
	if err != nil || out.Source != "cache" {
		t.Fatalf("first call: %+v %v", out, err)
	}
	if tr.Available() || !strings.Contains(tr.Status(), "credit balance") {
		t.Fatalf("status = %q", tr.Status())
	}
	if _, err := tr.Translate(context.Background(), "Failed logins in the last 24 hours"); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 {
		t.Fatalf("API called %d times, want 1", llm.calls)
	}
	if tr.APIKey() != "" {
		t.Fatal("agent mode must not get a key for a disabled LLM")
	}
}
