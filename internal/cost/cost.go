// Package cost prices plans before they run and enforces budgets while they
// run. Each location bills the way its real counterpart does: object-store
// query engines per TB scanned, some SIEM tiers per GB queried.
package cost

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
)

// Estimator sizes one slice (engine-specific: footer stats, file sizes, _count).
type Estimator interface {
	Estimate(ctx context.Context, s *planner.Slice, q *ir.Query) (planner.Estimate, error)
}

// Price converts bytes to dollars under a location's cost model.
func Price(m catalog.CostModel, bytes int64) float64 {
	switch m.Kind {
	case "per_tb_scanned":
		return float64(bytes) / 1e12 * m.Rate
	case "per_gb_scanned":
		return float64(bytes) / 1e9 * m.Rate
	}
	return 0
}

// Apply estimates every slice and the plan total.
func Apply(ctx context.Context, p *planner.Plan, est map[string]Estimator) error {
	p.Total = planner.Estimate{}
	for _, s := range p.Slices {
		e := est[s.Kind]
		if e == nil {
			return fmt.Errorf("no estimator for %s", s.Kind)
		}
		x, err := e.Estimate(ctx, s, p.Query)
		if err != nil {
			return fmt.Errorf("%s: estimate: %w", s.ID, err)
		}
		x.USD = Price(s.Loc.Cost, x.Bytes)
		s.Estimate = x
		p.Total.Bytes += x.Bytes
		p.Total.Rows += x.Rows
		p.Total.USD += x.USD
	}
	p.Total.Method = "sum of slices"
	return nil
}

// Decision is the budget verdict for a plan.
type Decision struct {
	Allowed         bool    `json:"allowed"`
	NeedsConfirm    bool    `json:"needs_confirm"`
	Reason          string  `json:"reason,omitempty"`
	EstimatedUSD    float64 `json:"estimated_usd"`
	SpentLastHour   float64 `json:"spent_last_hour_usd"`
	PerQueryCapUSD  float64 `json:"per_query_cap_usd"`
	PerHourCapUSD   float64 `json:"per_hour_cap_usd"`
	ConfirmAboveUSD float64 `json:"confirm_above_usd"`
}

// Ledger tracks spend per principal over a rolling hour.
type Ledger struct {
	mu    sync.Mutex
	spend map[string][]entry
	now   func() time.Time
}

type entry struct {
	at  time.Time
	usd float64
}

func NewLedger() *Ledger { return &Ledger{spend: map[string][]entry{}, now: time.Now} }

func (l *Ledger) Spent(principal string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.now().Add(-time.Hour)
	var keep []entry
	var sum float64
	for _, e := range l.spend[principal] {
		if e.at.After(cut) {
			keep = append(keep, e)
			sum += e.usd
		}
	}
	l.spend[principal] = keep
	return sum
}

func (l *Ledger) Charge(principal string, usd float64) {
	if usd <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spend[principal] = append(l.spend[principal], entry{l.now(), usd})
}

// Check applies the guardrails in order: per-query cap, rolling per-principal
// budget, then the confirmation threshold.
func Check(b config.Budget, perHour float64, l *Ledger, principal string, estUSD float64) Decision {
	if perHour <= 0 {
		perHour = b.PerHourUSD
	}
	d := Decision{Allowed: true, EstimatedUSD: estUSD, SpentLastHour: l.Spent(principal),
		PerQueryCapUSD: b.MaxPerQueryUSD, PerHourCapUSD: perHour, ConfirmAboveUSD: b.ConfirmAboveUSD}
	switch {
	case estUSD > b.MaxPerQueryUSD:
		d.Allowed, d.Reason = false, fmt.Sprintf("estimate $%.4f exceeds the per-query cap $%.2f", estUSD, b.MaxPerQueryUSD)
	case d.SpentLastHour+estUSD > perHour:
		d.Allowed, d.Reason = false, fmt.Sprintf("would exceed hourly budget: spent $%.4f + $%.4f > $%.2f", d.SpentLastHour, estUSD, perHour)
	case estUSD > b.ConfirmAboveUSD:
		d.NeedsConfirm, d.Reason = true, fmt.Sprintf("estimate $%.4f is above the confirmation threshold $%.2f", estUSD, b.ConfirmAboveUSD)
	}
	return d
}
