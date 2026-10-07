// Package duckdb runs slices over Parquet (and NDJSON in lite mode) with an
// embedded DuckDB, the stand-in for Athena-style SQL over object storage.
package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/marcboeker/go-duckdb" // registers the "duckdb" driver

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/engine"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/result"
)

const pageRows = 1000

type Adapter struct {
	db   *sql.DB
	kind string
}

// Open creates one in-process DuckDB shared by all DuckDB-backed sources and
// registers S3 credentials for every s3:// source (scoped per bucket).
func Open(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	needS3 := false
	for _, s := range cfg.Sources {
		if (s.Kind == "parquet" || s.Kind == "ndjson") && strings.HasPrefix(s.Root, "s3://") {
			needS3 = true
		}
	}
	if needS3 {
		if _, err := db.ExecContext(ctx, "INSTALL httpfs; LOAD httpfs;"); err != nil {
			return nil, fmt.Errorf("duckdb httpfs (needs internet once to install): %w", err)
		}
		for _, s := range cfg.Sources {
			if s.S3 == nil || !strings.HasPrefix(s.Root, "s3://") {
				continue
			}
			bucket := strings.SplitN(strings.TrimPrefix(s.Root, "s3://"), "/", 2)[0]
			region := s.S3.Region
			if region == "" {
				region = "us-east-1"
			}
			stmt := fmt.Sprintf(`CREATE OR REPLACE SECRET %s (TYPE S3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', REGION '%s', URL_STYLE 'path', USE_SSL %v, SCOPE 's3://%s')`,
				"s3_"+s.Name, esc(s.S3.AccessKey), esc(s.S3.SecretKey), esc(s.S3.Endpoint), esc(region), s.S3.UseSSL, esc(bucket))
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return nil, fmt.Errorf("duckdb s3 secret for %s: %w", s.Name, err)
			}
		}
	}
	return db, nil
}

func esc(s string) string { return strings.ReplaceAll(s, "'", "''") }

// New returns an adapter for one source kind ("parquet" or "ndjson").
func New(db *sql.DB, kind string) *Adapter { return &Adapter{db: db, kind: kind} }

func (a *Adapter) Kind() string { return a.kind }

// Estimate prices a slice before it runs. Parquet: compressed bytes of the
// columns the query needs, summed over whole pruned partitions (footer stats)
// — a conservative upper bound, since DuckDB may skip row groups too.
// NDJSON: whole file sizes, because row formats cannot skip columns.
func (a *Adapter) Estimate(_ context.Context, s *planner.Slice, q *ir.Query) (planner.Estimate, error) {
	cols := engine.NeededPhysical(s, q)
	var e planner.Estimate
	for _, p := range s.Parts {
		e.Rows += p.Rows
		if a.kind == "ndjson" {
			e.Bytes += p.Bytes
			continue
		}
		e.Bytes += columnBytes(p.ColumnBytes, cols, p.Bytes, s.Loc)
	}
	if a.kind == "ndjson" {
		e.Method = "file sizes (row format: every column is read)"
	} else {
		e.Method = fmt.Sprintf("Parquet footer stats: %d of %d columns, %d partitions", len(cols), len(s.Loc.Bindings), len(s.Parts))
	}
	return e, nil
}

func columnBytes(byCol map[string]int64, cols []string, total int64, loc *catalog.Location) int64 {
	if len(byCol) == 0 {
		// Partition newer than the catalog: assume an even split across columns.
		n := len(loc.Bindings) + len(loc.Unmapped)
		if n == 0 {
			return total
		}
		return total * int64(len(cols)) / int64(n)
	}
	var b int64
	for _, c := range cols {
		b += byCol[c]
	}
	return b
}

// scannedBytes is the accounting reported after execution: like Estimate but
// with row-group pruning on time, which is what DuckDB applies from footer
// min/max statistics.
func (a *Adapter) scannedBytes(s *planner.Slice, q *ir.Query) int64 {
	cols := engine.NeededPhysical(s, q)
	var b int64
	for _, p := range s.Parts {
		if a.kind == "ndjson" {
			b += p.Bytes
			continue
		}
		if len(p.RowGroups) == 0 {
			b += columnBytes(nil, cols, p.Bytes, s.Loc)
			continue
		}
		for _, rg := range p.RowGroups {
			if !rg.TimeMin.IsZero() && (rg.TimeMax.Before(s.Time.From) || !rg.TimeMin.Before(s.Time.To)) {
				continue // pruned by min/max
			}
			b += columnBytes(rg.ColumnBytes, cols, 0, s.Loc)
		}
	}
	return b
}

func (a *Adapter) Produce(ctx context.Context, s *planner.Slice, q *ir.Query, emit func(engine.Page) error) error {
	if s.SQL == nil {
		return fmt.Errorf("%s: slice not compiled to SQL", s.ID)
	}
	bytes := a.scannedBytes(s, q)
	rows, err := a.db.QueryContext(ctx, s.SQL.Text)
	if err != nil {
		return fmt.Errorf("duckdb: %w", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	if !q.IsAggregate() {
		page := engine.Page{Bytes: bytes}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			r := result.Row{Location: s.Location, Slice: s.ID, Fields: map[string]any{}}
			r.Time = time.UnixMilli(toInt(vals[0])).UTC()
			r.ID = fmt.Sprint(vals[1])
			for i, path := range s.SQL.Columns {
				v := normalize(vals[i+2])
				if b := s.Loc.Bindings[path]; b != nil {
					v = b.FromPhysical(v)
				}
				r.Fields[path] = v
			}
			page.Rows = append(page.Rows, r)
			if len(page.Rows) >= pageRows {
				if err := emit(page); err != nil {
					return err
				}
				page = engine.Page{Bytes: bytes}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		page.Done = true
		return emit(page)
	}

	page := engine.Page{Bytes: bytes, Done: true}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		g := &result.Group{}
		for _, path := range s.SQL.GroupCols {
			v := normalize(vals[idx[path]])
			if path == "time" {
				v = time.UnixMilli(toInt(v)).UTC()
			} else if b := s.Loc.Bindings[path]; b != nil {
				v = b.FromPhysical(v)
			}
			g.Key = append(g.Key, v)
		}
		for i, ag := range q.Aggs {
			ac := s.SQL.Aggs[i]
			st := result.NewState(ag.Fn)
			if ac.Count != "" {
				st.Count = toInt(vals[idx[ac.Count]])
			}
			if ac.Sum != "" {
				st.Sum = toFloat(vals[idx[ac.Sum]])
			}
			if ac.Min != "" {
				st.Min = aggValue(vals[idx[ac.Min]], ag.Field)
			}
			if ac.Max != "" {
				st.Max = aggValue(vals[idx[ac.Max]], ag.Field)
			}
			if ac.Distinct != "" {
				list, _ := vals[idx[ac.Distinct]].([]any)
				for _, v := range list {
					if len(st.Values) >= result.DistinctCap {
						st.Capped = true
						break
					}
					st.Values[fmt.Sprint(v)] = struct{}{}
				}
			}
			g.States = append(g.States, st)
		}
		page.Groups = append(page.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	page.Capped = len(q.GroupBy) > 0 && len(page.Groups) >= s.EngineLimit
	return emit(page)
}

func aggValue(v any, field string) any {
	v = normalize(v)
	if field == "time" && v != nil {
		return time.UnixMilli(toInt(v)).UTC()
	}
	return v
}

func normalize(v any) any {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC()
	}
	return v
}

func toInt(v any) int64 {
	switch x := normalize(v).(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func toFloat(v any) float64 {
	switch x := normalize(v).(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
