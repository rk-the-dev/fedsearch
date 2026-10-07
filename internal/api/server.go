// Package api serves the REST + SSE API and the embedded investigation
// console. Planning is a separate, free, side-effect-free call so the UI can
// show cost and pruning before anything runs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/exec"
	"github.com/rksurwase/fedsearch/internal/investigate"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/service"
	"github.com/rksurwase/fedsearch/internal/tools"
)

type Server struct {
	Svc    *service.Service
	Static fs.FS
	Mode   string // lite | docker (shown in the UI)
	Log    *slog.Logger
}

const principal = "analyst" // single-user POC; real deployments authenticate

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/catalog", s.catalog)
	mux.HandleFunc("POST /api/catalog/refresh", s.refresh)
	mux.HandleFunc("GET /api/questions", s.questions)
	mux.HandleFunc("POST /api/nl", s.nl)
	mux.HandleFunc("POST /api/plan", s.plan)
	mux.HandleFunc("POST /api/jobs", s.submit)
	mux.HandleFunc("GET /api/jobs/{id}", s.job)
	mux.HandleFunc("POST /api/jobs/{id}/confirm", s.confirm)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.cancel)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.events)
	mux.HandleFunc("POST /api/investigate", s.investigate)
	mux.HandleFunc("GET /api/audit", s.audit)
	if s.Static != nil {
		mux.Handle("/", http.FileServerFS(s.Static))
	}
	return s.logged(mux)
}

func (s *Server) logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		h.ServeHTTP(w, r)
		if s.Log != nil && strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasSuffix(r.URL.Path, "/events") {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "ms", time.Since(t0).Milliseconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps validation and planning errors to 400 with every problem listed.
func writeErr(w http.ResponseWriter, err error) {
	var ve *ir.ValidationError
	var rae *planner.ResidualAggregateError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, 400, map[string]any{"error": "invalid query", "problems": ve.Problems})
	case errors.As(err, &rae):
		writeJSON(w, 400, map[string]any{"error": err.Error(), "kind": "residual_aggregate"})
	case strings.HasPrefix(err.Error(), "budget:"):
		writeJSON(w, 402, map[string]any{"error": err.Error(), "kind": "budget"})
	default:
		writeJSON(w, 500, map[string]any{"error": err.Error()})
	}
}

type catalogView struct {
	*catalog.Catalog
	Overlaps     map[string][]catalog.Overlap `json:"overlaps"`
	Effective    map[string]any               `json:"effective"`
	Mode         string                       `json:"mode"`
	LLM          bool                         `json:"llm_available"`
	LLMStatus    string                       `json:"llm_status"`
	Now          time.Time                    `json:"now"`
	BudgetConfig any                          `json:"budget"`
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	cat := s.Svc.Catalog()
	now := s.Svc.Now()
	v := catalogView{Catalog: cat, Overlaps: map[string][]catalog.Overlap{}, Effective: map[string]any{}, Mode: s.Mode,
		LLM: s.Svc.NL.Available(), LLMStatus: s.Svc.NL.Status(), Now: now, BudgetConfig: s.Svc.Cfg.Budget}
	for name := range cat.Datasets {
		v.Overlaps[name] = cat.Overlaps(name)
	}
	for _, l := range cat.Locations {
		v.Effective[l.ID] = l.Coverage.Effective(now)
	}
	writeJSON(w, 200, v)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	rep, err := s.Svc.Refresh(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) questions(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	if g := s.Svc.NL.Golden; g != nil {
		for _, e := range g.Entries {
			out = append(out, map[string]any{"question": e.Question, "demo": e.Demo})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) nl(w http.ResponseWriter, r *http.Request) {
	var in struct{ Question string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"error": "expected {\"question\": ...}"})
		return
	}
	tr, err := s.Svc.Ask(r.Context(), in.Question, principal, "ui")
	if err != nil {
		resp := map[string]any{"error": err.Error()}
		if tr != nil {
			resp["attempts"] = tr.Attempts
		}
		writeJSON(w, 422, resp)
		return
	}
	writeJSON(w, 200, tr)
}

type queryReq struct {
	Query json.RawMessage `json:"query"`
	Naive bool            `json:"naive"`
}

func (s *Server) planFrom(ctx context.Context, body queryReq) (*planner.Plan, error) {
	q, err := s.Svc.ParseQuery(body.Query)
	if err != nil {
		return nil, err
	}
	return s.Svc.PlanWith(ctx, q, body.Naive)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var body queryReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, err := s.planFrom(r.Context(), body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"plan": p, "decision": s.Svc.Decide(p, principal, 0)})
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body queryReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, err := s.planFrom(r.Context(), body)
	if err != nil {
		writeErr(w, err)
		return
	}
	j, err := s.Svc.Submit(context.Background(), p, service.SubmitOptions{Principal: principal, Surface: "ui"})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j.Snapshot())
}

func (s *Server) jobOr404(w http.ResponseWriter, r *http.Request) *exec.Job {
	j := s.Svc.Jobs.Get(r.PathValue("id"))
	if j == nil {
		writeJSON(w, 404, map[string]any{"error": "no such job"})
	}
	return j
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	if j := s.jobOr404(w, r); j != nil {
		writeJSON(w, 200, j.Snapshot())
	}
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.Jobs.Confirm(r.PathValue("id"), r.URL.Query().Get("reject") == ""); err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.Jobs.Cancel(r.PathValue("id")); err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}

// events streams a job's events as Server-Sent Events: replay, then live.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	j := s.jobOr404(w, r)
	if j == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	past, live, unsub := j.Subscribe()
	defer unsub()
	send := func(ev exec.Event) bool {
		b, err := json.Marshal(ev.Data)
		if err != nil {
			return true
		}
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b)
		fl.Flush()
		return ev.Type != "done"
	}
	for _, ev := range past {
		if !send(ev) {
			return
		}
	}
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case ev := <-live:
			if !send(ev) {
				return
			}
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) investigate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Indicator string `json:"indicator"`
		Window    string `json:"window"`
		Mode      string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Indicator == "" {
		writeJSON(w, 400, map[string]any{"error": "expected {\"indicator\": \"<ip>\"}"})
		return
	}
	if in.Window == "" {
		in.Window = "now-180d"
	}
	p, err := s.agentPrincipal()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	hub := tools.NewHub(s.Svc, "investigate")
	var rep *investigate.Report
	if in.Mode == "agent" {
		key := s.Svc.NL.APIKey()
		if key == "" {
			writeJSON(w, 400, map[string]any{"error": "agent mode needs a working LLM (" + s.Svc.NL.Status() + "); use the playbook"})
			return
		}
		a := &investigate.Agent{APIKey: key, Model: s.Svc.Cfg.LLM.Model, BaseURL: s.Svc.Cfg.LLM.BaseURL}
		rep, err = a.Run(r.Context(), hub, p, in.Indicator, in.Window)
	} else {
		hub.MaxRows = 2000 // deterministic code, not an LLM: it may read full results
		rep, err = investigate.Playbook(r.Context(), hub, p, in.Indicator, in.Window)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) agentPrincipal() (tools.Principal, error) {
	if len(s.Svc.Cfg.Agents) == 0 {
		return tools.Principal{}, errors.New("no agent principal configured")
	}
	return tools.PrincipalFrom(s.Svc.Cfg.Agents[0]), nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	recs, err := s.Svc.Audit.Recent(n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, recs)
}
