package catalog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/guard"
	"github.com/rksurwase/fedsearch/internal/objstore"
	"github.com/rksurwase/fedsearch/internal/osclient"
	"github.com/rksurwase/fedsearch/internal/schema"
)

const (
	sampleRows     = 2000
	topValuesMax   = 12 // low-cardinality threshold for listing top values
	examplesMax    = 4
	exampleMaxRune = 64
)

// sampleLocation reads up to sampleRows physical rows from the newest
// partition. Stats feed NL grounding, so they describe recent data.
func sampleLocation(ctx context.Context, src config.Source, loc *Location) ([]map[string]any, string, error) {
	if len(loc.Partitions) == 0 {
		return nil, "", nil
	}
	newest := loc.Partitions[len(loc.Partitions)-1]
	switch loc.Kind {
	case "parquet":
		rows, err := sampleParquet(ctx, src, newest)
		return rows, newest.Key, err
	case "ndjson":
		rows, err := sampleNDJSON(ctx, src, newest)
		return rows, newest.Key, err
	case "opensearch":
		c := osclient.New(src.URL)
		resp, err := c.Search(ctx, []string{newest.Key}, map[string]any{"size": sampleRows, "track_total_hits": false})
		if err != nil {
			return nil, "", err
		}
		var rows []map[string]any
		for _, h := range resp.Hits.Hits {
			var m map[string]any
			if json.Unmarshal(h.Source, &m) == nil {
				rows = append(rows, m)
			}
		}
		return rows, newest.Key, nil
	}
	return nil, "", fmt.Errorf("no sampler for %s", loc.Kind)
}

func sampleParquet(ctx context.Context, src config.Source, p *Partition) ([]map[string]any, error) {
	if len(p.Objects) == 0 {
		return nil, nil
	}
	store, err := objstore.Open(src.Root, src.S3)
	if err != nil {
		return nil, err
	}
	key := objectKey(src, p.Objects[0])
	objs, err := store.List(ctx, parentDir(key))
	if err != nil {
		return nil, err
	}
	var size int64
	for _, o := range objs {
		if o.Key == key {
			size = o.Size
		}
	}
	r, err := store.ReaderAt(ctx, key, size)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := parquet.OpenFile(r, size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return nil, err
	}
	cols := f.Schema().Columns()
	rr := parquet.NewReader(f)
	defer rr.Close()
	buf := make([]parquet.Row, 256)
	var out []map[string]any
	for len(out) < sampleRows {
		n, err := rr.ReadRows(buf)
		for _, row := range buf[:n] {
			m := map[string]any{}
			for _, v := range row {
				ci := v.Column()
				if ci < 0 || ci >= len(cols) || v.IsNull() {
					continue
				}
				name := strings.Join(cols[ci], ".")
				switch v.Kind() {
				case parquet.ByteArray, parquet.FixedLenByteArray:
					m[name] = string(v.ByteArray())
				case parquet.Int32:
					m[name] = int64(v.Int32())
				case parquet.Int64:
					if name == "time" {
						m[name] = time.UnixMilli(v.Int64()).UTC()
					} else {
						m[name] = v.Int64()
					}
				case parquet.Double:
					m[name] = v.Double()
				case parquet.Float:
					m[name] = float64(v.Float())
				case parquet.Boolean:
					m[name] = v.Boolean()
				}
			}
			out = append(out, m)
		}
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func sampleNDJSON(ctx context.Context, src config.Source, p *Partition) ([]map[string]any, error) {
	if len(p.Objects) == 0 {
		return nil, nil
	}
	store, err := objstore.Open(src.Root, src.S3)
	if err != nil {
		return nil, err
	}
	key := objectKey(src, p.Objects[0])
	r, err := store.ReaderAt(ctx, key, p.Bytes)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	sc := bufio.NewScanner(io.NewSectionReader(r, 0, p.Bytes))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var out []map[string]any
	for sc.Scan() && len(out) < sampleRows {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if _, ok := m["event_id"]; ok {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// objectKey maps an engine URI back to a key relative to the source root.
func objectKey(src config.Source, uri string) string {
	root := strings.TrimRight(src.Root, "/")
	if strings.HasPrefix(uri, "s3://") {
		// s3://bucket/base/<key> with root s3://bucket/base
		return strings.TrimPrefix(strings.TrimPrefix(uri, root), "/")
	}
	if abs, err := absPath(root); err == nil {
		return strings.TrimPrefix(strings.TrimPrefix(uri, abs), "/")
	}
	return uri
}

func parentDir(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[:i]
	}
	return ""
}

// computeStats summarizes sampled rows per OCSF field. Low-cardinality
// dimensions list their top values (what the NL layer needs to avoid guessing
// "deny" when the data says "blocked"); everything else gets a distinct
// count and a few sanitized examples. Free-text fields never list top values,
// and suspicious values are withheld.
func computeStats(ds *Dataset, loc *Location, rows []map[string]any) map[string]*FieldStats {
	out := map[string]*FieldStats{}
	if len(rows) == 0 {
		return out
	}
	for _, f := range ds.Fields {
		b := loc.Bindings[f.Path]
		if b == nil || f.Role == schema.RoleEventTime || f.Role == schema.RoleEventID {
			continue
		}
		counts := map[string]int{}
		nulls := 0
		for _, r := range rows {
			v, ok := r[b.Physical]
			if !ok || v == nil {
				nulls++
				continue
			}
			v = b.FromPhysical(normalizeJSONNumber(v))
			counts[fmt.Sprint(v)]++
		}
		st := &FieldStats{SampledRows: int64(len(rows)), Distinct: int64(len(counts)), NullFrac: float64(nulls) / float64(len(rows))}
		vals := make([]string, 0, len(counts))
		for v := range counts {
			vals = append(vals, v)
		}
		sort.Slice(vals, func(i, j int) bool {
			if counts[vals[i]] != counts[vals[j]] {
				return counts[vals[i]] > counts[vals[j]]
			}
			return vals[i] < vals[j]
		})
		if f.Role != schema.RoleFreeText && len(vals) <= topValuesMax && f.Type != schema.IP {
			st.TopValues = vals
		} else {
			for _, v := range vals {
				if len(st.Examples) >= examplesMax {
					break
				}
				s := Sample{Value: guard.Sanitize(v, exampleMaxRune)}
				if guard.Suspicious(v) {
					s = Sample{Value: guard.Redacted, Suspicious: true}
				}
				st.Examples = append(st.Examples, s)
			}
		}
		out[f.Path] = st
	}
	return out
}

func normalizeJSONNumber(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

func absPath(p string) (string, error) { return filepath.Abs(p) }
