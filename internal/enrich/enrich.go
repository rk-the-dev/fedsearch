// Package enrich joins result rows with asset context — which host and owner
// held an IP — as of each event's timestamp (the Reef stand-in).
//
// The same IP means different hosts over time (DHCP, autoscaling, VPN pools).
// Joining on the current owner attributes old activity to whoever holds the
// IP today; for an investigation that is worse than no answer. Both views are
// returned so the UI can show the difference, but as_of is the answer.
package enrich

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rksurwase/fedsearch/internal/result"
)

type Host struct {
	Hostname    string `json:"hostname"`
	Owner       string `json:"owner"`
	Department  string `json:"department"`
	Kind        string `json:"kind,omitempty"`
	Criticality string `json:"criticality"`
}

// Assignment: IP belonged to Host during [From, To); zero To = still assigned.
type Assignment struct {
	IP   string    `json:"ip"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Host Host      `json:"host"`
}

func (a Assignment) Covers(t time.Time) bool {
	return !t.Before(a.From) && (a.To.IsZero() || t.Before(a.To))
}

// Store returns every assignment interval for the given IPs.
type Store interface {
	Assignments(ctx context.Context, ips []string) (map[string][]Assignment, error)
	Close() error
}

// Context is the enrichment attached to one field of one row.
type Context struct {
	AsOf    *Host `json:"as_of"`   // who held the IP at event time (correct)
	Current *Host `json:"current"` // who holds it now (naive join)
	Changed bool  `json:"changed"` // as_of and current disagree
}

// Enricher batches lookups (one store query per batch of rows) and caches
// interval lists. Closed intervals never change, so only lists containing an
// open-ended interval expire.
type Enricher struct {
	Store Store
	TTL   time.Duration
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	list []Assignment
	at   time.Time
	open bool
}

func New(s Store) *Enricher {
	return &Enricher{Store: s, TTL: 5 * time.Minute, cache: map[string]cached{}}
}

// Rows enriches the given IP fields of every row in place.
func (e *Enricher) Rows(ctx context.Context, rows []result.Row, fields []string) error {
	if e == nil || e.Store == nil || len(fields) == 0 || len(rows) == 0 {
		return nil
	}
	ips := map[string]bool{}
	for _, r := range rows {
		for _, f := range fields {
			if ip, ok := r.Fields[f].(string); ok && ip != "" {
				ips[ip] = true
			}
		}
	}
	intervals, err := e.lookup(ctx, ips)
	if err != nil {
		return err
	}
	for i := range rows {
		for _, f := range fields {
			ip, ok := rows[i].Fields[f].(string)
			if !ok {
				continue
			}
			c := Context{}
			for _, a := range intervals[ip] {
				h := a.Host
				if a.Covers(rows[i].Time) {
					c.AsOf = &h
				}
				if a.To.IsZero() {
					c.Current = &h
				}
			}
			if c.AsOf == nil && c.Current == nil {
				continue // external IP: no context
			}
			c.Changed = hostName(c.AsOf) != hostName(c.Current)
			if rows[i].Context == nil {
				rows[i].Context = map[string]any{}
			}
			rows[i].Context[f] = c
		}
	}
	return nil
}

// Lookup returns the as-of and current host for one IP at one time.
func (e *Enricher) Lookup(ctx context.Context, ip string, at time.Time) (Context, []Assignment, error) {
	m, err := e.lookup(ctx, map[string]bool{ip: true})
	if err != nil {
		return Context{}, nil, err
	}
	c := Context{}
	for _, a := range m[ip] {
		h := a.Host
		if a.Covers(at) {
			c.AsOf = &h
		}
		if a.To.IsZero() {
			c.Current = &h
		}
	}
	c.Changed = hostName(c.AsOf) != hostName(c.Current)
	return c, m[ip], nil
}

func (e *Enricher) lookup(ctx context.Context, ips map[string]bool) (map[string][]Assignment, error) {
	out := map[string][]Assignment{}
	var miss []string
	now := time.Now()
	e.mu.Lock()
	for ip := range ips {
		if c, ok := e.cache[ip]; ok && (!c.open || now.Sub(c.at) < e.TTL) {
			out[ip] = c.list
		} else {
			miss = append(miss, ip)
		}
	}
	e.mu.Unlock()
	if len(miss) == 0 {
		return out, nil
	}
	got, err := e.Store.Assignments(ctx, miss)
	if err != nil {
		return nil, fmt.Errorf("context store: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ip := range miss {
		list := got[ip]
		open := false
		for _, a := range list {
			open = open || a.To.IsZero()
		}
		e.cache[ip] = cached{list: list, at: now, open: open}
		out[ip] = list
	}
	return out, nil
}

func hostName(h *Host) string {
	if h == nil {
		return ""
	}
	return h.Hostname
}
