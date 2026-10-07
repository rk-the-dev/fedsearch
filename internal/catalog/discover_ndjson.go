package catalog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/objstore"
	"github.com/rksurwase/fedsearch/internal/timex"
)

// ndjsonDiscoverer learns a directory of daily newline-delimited JSON files
// (the hot tier in lite mode, read by DuckDB). JSON has no footer, so a cache
// miss means scanning the file; unchanged files are skipped by ETag.
type ndjsonDiscoverer struct{}

func (ndjsonDiscoverer) Discover(ctx context.Context, src config.Source, dataset string, prev *Location) (*Location, *LocationReport, error) {
	start := time.Now()
	store, err := objstore.Open(src.Root, src.S3)
	if err != nil {
		return nil, nil, err
	}
	objs, err := store.List(ctx, dataset)
	if err != nil {
		return nil, nil, err
	}
	loc := &Location{
		ID: src.Name + "." + dataset, Dataset: dataset,
		Layout: Layout{Root: store.URI(dataset), Pattern: "*-YYYY-MM-DD.ndjson"},
		Caps:   Capabilities{Engine: "duckdb", Dialect: "duckdb_sql", UnsupportedOp: []string{"cidr"}, Aggregations: true},
	}
	rep := &LocationReport{Location: loc.ID}
	cached := map[string]*Partition{}
	if prev != nil {
		for _, p := range prev.Partitions {
			cached[p.Key] = p
		}
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".ndjson") && !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		key := path.Base(o.Key)
		span, ok := daySpan(key)
		if !ok {
			continue
		}
		if p := cached[key]; p != nil && p.ETag == o.ETag {
			loc.Partitions = append(loc.Partitions, p)
			continue
		}
		p, err := scanNDJSON(ctx, store, o, span)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", o.Key, err)
		}
		rep.PartitionsRead++
		rep.FilesRead++
		rep.MetadataBytes += o.Size
		rep.FileBytes += o.Size
		loc.Partitions = append(loc.Partitions, p)
	}
	rep.Partitions = len(loc.Partitions)
	drift, err := finish(loc, src, prev, time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	rep.Drift = drift
	rep.Duration = time.Since(start)
	return loc, rep, nil
}

func scanNDJSON(ctx context.Context, store objstore.Store, o objstore.Object, span timex.Range) (*Partition, error) {
	p := &Partition{Key: path.Base(o.Key), Span: span, ETag: o.ETag, Bytes: o.Size, Objects: []string{store.URI(o.Key)}}
	r, err := store.ReaderAt(ctx, o.Key, o.Size)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	sc := bufio.NewScanner(io.NewSectionReader(r, 0, o.Size))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var tmin, tmax time.Time
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || strings.HasPrefix(string(line), `{"index"`) {
			continue // OpenSearch _bulk action line
		}
		var doc map[string]any
		if err := json.Unmarshal(line, &doc); err != nil {
			return nil, err
		}
		if _, ok := doc["event_id"]; !ok {
			continue
		}
		p.Rows++
		if len(p.Schema) == 0 {
			p.Schema = jsonSchema(doc)
		}
		if ts, ok := doc["time"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if tmin.IsZero() || t.Before(tmin) {
					tmin = t
				}
				if t.After(tmax) {
					tmax = t
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	p.RowGroups = []RowGroup{{Rows: p.Rows, TimeMin: tmin.UTC(), TimeMax: tmax.UTC()}}
	return p, nil
}

func jsonSchema(doc map[string]any) []PhysicalField {
	var out []PhysicalField
	for _, k := range sortedKeys(doc) {
		t := "STRING"
		switch v := doc[k].(type) {
		case float64:
			t = "BIGINT"
			if v != float64(int64(v)) {
				t = "DOUBLE"
			}
		case bool:
			t = "BOOLEAN"
		case string:
			if k == "time" {
				t = "TIMESTAMP"
			}
		}
		out = append(out, PhysicalField{Name: k, Type: t})
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
