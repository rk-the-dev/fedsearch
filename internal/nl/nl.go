// Package nl turns a natural-language question into IR. The LLM never writes
// executable query text: it fills a typed IR through a forced tool call,
// deterministic code validates it, and validation errors go back to the LLM
// for at most two repairs. A golden cache of known-good IR answers known
// questions deterministically (demos, tests, offline).
package nl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/ir"
)

// Translation is the NL layer's answer, with its evidence.
type Translation struct {
	Question    string    `json:"question"`
	Query       *ir.Query `json:"ir"`
	Explanation string    `json:"explanation"`
	Source      string    `json:"source"` // llm | cache
	Attempts    []Attempt `json:"attempts,omitempty"`
	LatencyMS   int64     `json:"latency_ms"`
}

type Attempt struct {
	Raw      json.RawMessage `json:"raw"`
	Problems []string        `json:"problems,omitempty"`
}

// Translator holds the LLM client and the golden cache.
type Translator struct {
	Cfg    config.LLM
	LLM    LLM
	Golden *Golden
	// Prepare normalizes and validates against the live catalog.
	Prepare func(*ir.Query) error
	Catalog func() *catalog.Catalog
}

var ErrNoLLM = errors.New("no LLM configured and the question is not in the golden set")

// New builds a translator. The LLM is optional; without an API key the
// translator answers from the golden cache only.
func New(cfg config.LLM, golden *Golden, cat func() *catalog.Catalog, prepare func(*ir.Query) error) *Translator {
	t := &Translator{Cfg: cfg, Golden: golden, Catalog: cat, Prepare: prepare}
	if cfg.Provider == "anthropic" {
		envName := cfg.APIKeyEnv
		if envName == "" {
			envName = "ANTHROPIC_API_KEY"
		}
		if key := os.Getenv(envName); key != "" {
			t.LLM = &Anthropic{APIKey: key, Model: cfg.Model, BaseURL: cfg.BaseURL}
		}
	}
	return t
}

// Available reports whether an LLM is configured.
func (t *Translator) Available() bool { return t.LLM != nil }

func (t *Translator) Translate(ctx context.Context, question string) (*Translation, error) {
	start := time.Now()
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("empty question")
	}
	mode := t.Cfg.Mode
	fromCache := func() (*Translation, error) {
		if t.Golden == nil {
			return nil, ErrNoLLM
		}
		e, ok := t.Golden.Lookup(question)
		if !ok {
			return nil, ErrNoLLM
		}
		var q ir.Query
		if err := json.Unmarshal(e.IR, &q); err != nil {
			return nil, err
		}
		if err := t.Prepare(&q); err != nil {
			return nil, fmt.Errorf("golden entry no longer valid: %w", err)
		}
		return &Translation{Question: question, Query: &q, Explanation: e.Explanation, Source: "cache", LatencyMS: time.Since(start).Milliseconds()}, nil
	}
	if mode == "cache_only" || t.LLM == nil {
		return fromCache()
	}
	if mode == "cache_first" {
		if tr, err := fromCache(); err == nil {
			return tr, nil
		}
	}
	tr, err := t.viaLLM(ctx, question)
	if err != nil {
		// Never strand the user: fall back to the golden answer if there is one.
		if c, cerr := fromCache(); cerr == nil {
			c.Attempts = tr.attemptsOrNil()
			return c, nil
		}
		return tr, err
	}
	tr.LatencyMS = time.Since(start).Milliseconds()
	return tr, nil
}

func (tr *Translation) attemptsOrNil() []Attempt {
	if tr == nil {
		return nil
	}
	return tr.Attempts
}

const maxRepairs = 2

func (t *Translator) viaLLM(ctx context.Context, question string) (*Translation, error) {
	tr := &Translation{Question: question, Source: "llm"}
	sys := SystemPrompt(t.Catalog(), ir.Now())
	conv := []Message{{Role: "user", Content: []Block{{Type: "text", Text: question}}}}
	for attempt := 0; attempt <= maxRepairs; attempt++ {
		call, err := t.LLM.EmitQuery(ctx, sys, conv)
		if err != nil {
			return tr, fmt.Errorf("llm: %w", err)
		}
		a := Attempt{Raw: call.Input}
		var payload struct {
			Explanation string          `json:"explanation"`
			Query       json.RawMessage `json:"query"`
		}
		var q ir.Query
		err = json.Unmarshal(call.Input, &payload)
		if err == nil {
			err = json.Unmarshal(payload.Query, &q)
		}
		if err == nil {
			err = t.Prepare(&q)
		}
		if err == nil {
			tr.Attempts = append(tr.Attempts, a)
			tr.Query, tr.Explanation = &q, payload.Explanation
			return tr, nil
		}
		var ve *ir.ValidationError
		if errors.As(err, &ve) {
			a.Problems = ve.Problems
		} else {
			a.Problems = []string{err.Error()}
		}
		tr.Attempts = append(tr.Attempts, a)
		// Repair turn: return the problems as an error tool_result.
		conv = append(conv,
			Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: call.ID, Name: toolName, Input: call.Input}}},
			Message{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: call.ID, IsError: true,
				Content: "The query failed validation. Fix every problem and call emit_query again:\n- " + strings.Join(a.Problems, "\n- ")}}},
		)
	}
	return tr, fmt.Errorf("llm output still invalid after %d repairs: %v", maxRepairs, tr.Attempts[len(tr.Attempts)-1].Problems)
}

// --- golden cache --------------------------------------------------------------

type GoldenEntry struct {
	Question    string          `json:"question"`
	Aliases     []string        `json:"aliases,omitempty"`
	IR          json.RawMessage `json:"ir"`
	Explanation string          `json:"explanation"`
	Demo        bool            `json:"demo,omitempty"`
}

type Golden struct {
	Entries []GoldenEntry
	index   map[string]int
}

func LoadGolden(path string) (*Golden, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []GoldenEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	g := &Golden{Entries: entries, index: map[string]int{}}
	for i, e := range entries {
		g.index[Normalize(e.Question)] = i
		for _, a := range e.Aliases {
			g.index[Normalize(a)] = i
		}
	}
	return g, nil
}

func (g *Golden) Lookup(question string) (GoldenEntry, bool) {
	i, ok := g.index[Normalize(question)]
	if !ok {
		return GoldenEntry{}, false
	}
	return g.Entries[i], true
}

var nonWord = regexp.MustCompile(`[^a-z0-9.]+`)

// Normalize makes cache keys insensitive to case, punctuation and spacing.
func Normalize(q string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(q), " "), " .")
}

// APIKey returns the configured Anthropic key (for the agent mode), or "".
func (t *Translator) APIKey() string {
	if a, ok := t.LLM.(*Anthropic); ok {
		return a.APIKey
	}
	return ""
}
