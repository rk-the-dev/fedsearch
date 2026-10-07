package catalog

import (
	"context"
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/objstore"
)

// parquetDiscoverer learns a Hive-partitioned Parquet layout
// (<dataset>/dt=YYYY-MM-DD/*.parquet) by listing objects and reading only
// file footers: schema, row counts, per-row-group time min/max and
// per-column compressed sizes. Unchanged files (same ETag) are not read.
type parquetDiscoverer struct{}

func (parquetDiscoverer) Discover(ctx context.Context, src config.Source, dataset string, prev *Location) (*Location, *LocationReport, error) {
	start := time.Now()
	store, err := objstore.Open(src.Root, src.S3)
	if err != nil {
		return nil, nil, err
	}
	objs, err := store.List(ctx, dataset)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", dataset, err)
	}
	loc := &Location{
		ID: src.Name + "." + dataset, Dataset: dataset,
		Layout: Layout{Root: store.URI(dataset), Pattern: "dt=YYYY-MM-DD/*.parquet"},
		Caps:   Capabilities{Engine: "duckdb", Dialect: "duckdb_sql", UnsupportedOp: []string{"cidr"}, Aggregations: true},
	}
	rep := &LocationReport{Location: loc.ID}

	// Group objects into partitions by their dt= directory.
	byPart := map[string][]objstore.Object{}
	var keys []string
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".parquet") {
			continue
		}
		dir := path.Dir(o.Key)
		if _, ok := byPart[dir]; !ok {
			keys = append(keys, dir)
		}
		byPart[dir] = append(byPart[dir], o)
	}
	cached := map[string]*Partition{}
	if prev != nil {
		for _, p := range prev.Partitions {
			cached[p.Key] = p
		}
	}

	for _, dir := range keys {
		key := path.Base(dir)
		span, ok := daySpan(key)
		if !ok {
			continue
		}
		files := byPart[dir]
		etag := combinedETag(files)
		if p := cached[key]; p != nil && p.ETag == etag {
			loc.Partitions = append(loc.Partitions, p)
			continue
		}
		p := &Partition{Key: key, Span: span, ETag: etag, ColumnBytes: map[string]int64{}}
		for _, f := range files {
			if err := readFooter(ctx, store, f, p, rep); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f.Key, err)
			}
			p.Objects = append(p.Objects, store.URI(f.Key))
		}
		rep.PartitionsRead++
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

func combinedETag(files []objstore.Object) string {
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = f.Key + "@" + f.ETag
	}
	return strings.Join(parts, "|")
}

// readFooter opens a Parquet file through a byte-counting ReaderAt. OpenFile
// reads the 8-byte trailer and the thrift footer; with the page index and
// bloom filters skipped, no data pages are touched.
func readFooter(ctx context.Context, store objstore.Store, obj objstore.Object, p *Partition, rep *LocationReport) error {
	r, err := store.ReaderAt(ctx, obj.Key, obj.Size)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := parquet.OpenFile(r, obj.Size,
		parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true), parquet.SkipMagicBytes(true),
		parquet.ReadBufferSize(8<<10))
	if err != nil {
		return err
	}
	rep.FilesRead++
	rep.MetadataBytes += r.BytesRead()
	rep.MetadataCalls += r.Calls()
	rep.FileBytes += obj.Size

	if len(p.Schema) == 0 {
		for _, fld := range f.Schema().Fields() {
			p.Schema = append(p.Schema, PhysicalField{Name: fld.Name(), Type: physType(fld)})
		}
	}
	md := f.Metadata()
	p.Bytes += obj.Size
	for _, rg := range md.RowGroups {
		g := RowGroup{Rows: rg.NumRows, ColumnBytes: map[string]int64{}}
		for _, c := range rg.Columns {
			name := strings.Join(c.MetaData.PathInSchema, ".")
			g.ColumnBytes[name] += c.MetaData.TotalCompressedSize
			p.ColumnBytes[name] += c.MetaData.TotalCompressedSize
			if name == "time" {
				g.TimeMin, g.TimeMax = statTime(c.MetaData.Statistics.MinValue), statTime(c.MetaData.Statistics.MaxValue)
			}
		}
		p.Rows += rg.NumRows
		p.RowGroups = append(p.RowGroups, g)
	}
	return nil
}

func physType(f parquet.Field) string {
	t := f.Type()
	if lt := t.LogicalType(); lt != nil && lt.Timestamp != nil {
		return "TIMESTAMP"
	}
	if lt := t.LogicalType(); lt != nil && lt.UTF8 != nil {
		return "STRING"
	}
	return t.String()
}

// statTime decodes a PLAIN-encoded INT64 millisecond timestamp statistic.
func statTime(b []byte) time.Time {
	if len(b) != 8 {
		return time.Time{}
	}
	return time.UnixMilli(int64(binary.LittleEndian.Uint64(b))).UTC()
}
