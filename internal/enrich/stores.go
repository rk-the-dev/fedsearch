package enrich

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"

	"github.com/rksurwase/fedsearch/internal/config"
)

// Open builds the configured context store.
func Open(cfg config.ContextStore) (Store, error) {
	switch cfg.Kind {
	case "csv":
		return LoadCSV(cfg.Dir)
	case "postgres":
		db, err := sql.Open("postgres", cfg.DSN)
		if err != nil {
			return nil, err
		}
		return &Postgres{DB: db}, nil
	case "", "none":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown context store %q", cfg.Kind)
}

// Memory is an in-process store loaded from the generator's CSVs (lite mode).
type Memory struct{ byIP map[string][]Assignment }

func (m *Memory) Close() error { return nil }

func (m *Memory) Assignments(_ context.Context, ips []string) (map[string][]Assignment, error) {
	out := map[string][]Assignment{}
	for _, ip := range ips {
		if l, ok := m.byIP[ip]; ok {
			out[ip] = l
		}
	}
	return out, nil
}

func LoadCSV(dir string) (*Memory, error) {
	read := func(name string) ([][]string, error) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		rows, err := csv.NewReader(f).ReadAll()
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[1:], nil // skip header
	}
	hosts, err := read("hosts.csv")
	if err != nil {
		return nil, err
	}
	byName := map[string]Host{}
	for _, r := range hosts {
		byName[r[0]] = Host{Hostname: r[0], Owner: r[1], Department: r[2], Kind: r[3], Criticality: r[4]}
	}
	assigns, err := read("ip_assignments.csv")
	if err != nil {
		return nil, err
	}
	m := &Memory{byIP: map[string][]Assignment{}}
	for _, r := range assigns {
		from, err := time.Parse(time.RFC3339, r[2])
		if err != nil {
			return nil, err
		}
		var to time.Time
		if r[3] != "" {
			if to, err = time.Parse(time.RFC3339, r[3]); err != nil {
				return nil, err
			}
		}
		m.byIP[r[0]] = append(m.byIP[r[0]], Assignment{IP: r[0], From: from.UTC(), To: to.UTC(), Host: byName[r[1]]})
	}
	return m, nil
}

// Postgres queries the temporal ip_assignments table. One query per batch:
// all intervals for the batch's distinct IPs.
type Postgres struct{ DB *sql.DB }

func (p *Postgres) Close() error { return p.DB.Close() }

func (p *Postgres) Assignments(ctx context.Context, ips []string) (map[string][]Assignment, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT host(a.ip), a.valid_from, a.valid_to, h.hostname, h.owner, h.department, h.kind, h.criticality
		FROM ip_assignments a JOIN hosts h USING (hostname)
		WHERE a.ip = ANY($1::inet[])
		ORDER BY a.ip, a.valid_from`, pq.Array(ips))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Assignment{}
	for rows.Next() {
		var a Assignment
		var to sql.NullTime
		if err := rows.Scan(&a.IP, &a.From, &to, &a.Host.Hostname, &a.Host.Owner, &a.Host.Department, &a.Host.Kind, &a.Host.Criticality); err != nil {
			return nil, err
		}
		a.From = a.From.UTC()
		if to.Valid {
			a.To = to.Time.UTC()
		}
		out[a.IP] = append(out[a.IP], a)
	}
	return out, rows.Err()
}

func configFor(dsn string) config.ContextStore {
	return config.ContextStore{Kind: "postgres", DSN: dsn}
}
