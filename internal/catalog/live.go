package catalog

import (
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/objstore"
	"github.com/rksurwase/fedsearch/internal/osclient"
	"github.com/rksurwase/fedsearch/internal/timex"
)

// ListLive lists the partitions of a location that intersect r, directly from
// the source at plan time. The catalog is a planning hint; this listing is
// the truth about which partitions exist, so data that landed after the last
// refresh is never silently skipped. Stats come from the catalog when it
// knows the partition, and are estimated from the location average when not
// (Partition.Stale = true).
func ListLive(ctx context.Context, src config.Source, loc *Location, r timex.Range) ([]*Partition, error) {
	known := map[string]*Partition{}
	for _, p := range loc.Partitions {
		known[p.Key] = p
	}
	estimate := func(key string, span timex.Range) *Partition {
		if p := known[key]; p != nil {
			return p
		}
		avgRows, avgBytes := int64(0), int64(0)
		if n := int64(len(loc.Partitions)); n > 0 {
			avgRows, avgBytes = loc.Rows/n, loc.Bytes/n
		}
		return &Partition{Key: key, Span: span, Rows: avgRows, Bytes: avgBytes, Stale: true}
	}

	var out []*Partition
	switch loc.Kind {
	case "parquet":
		store, err := objstore.Open(src.Root, src.S3)
		if err != nil {
			return nil, err
		}
		dirs, err := store.Dirs(ctx, loc.Dataset)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			span, ok := daySpan(path.Base(d))
			if !ok || !span.Overlaps(r) {
				continue
			}
			p := estimate(path.Base(d), span)
			if p.Stale || len(p.Objects) == 0 {
				objs, err := store.List(ctx, d)
				if err != nil {
					return nil, err
				}
				cp := *p
				cp.Objects = nil
				for _, o := range objs {
					if strings.HasSuffix(o.Key, ".parquet") {
						cp.Objects = append(cp.Objects, store.URI(o.Key))
					}
				}
				p = &cp
			}
			out = append(out, p)
		}
	case "ndjson":
		store, err := objstore.Open(src.Root, src.S3)
		if err != nil {
			return nil, err
		}
		objs, err := store.List(ctx, loc.Dataset)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			span, ok := daySpan(path.Base(o.Key))
			if !ok || !span.Overlaps(r) {
				continue
			}
			p := estimate(path.Base(o.Key), span)
			if p.Stale {
				cp := *p
				cp.Objects, cp.Bytes = []string{store.URI(o.Key)}, o.Size
				p = &cp
			}
			out = append(out, p)
		}
	case "opensearch":
		indices, err := osclient.New(src.URL).CatIndices(ctx, IndexPattern(src, loc.Dataset))
		if err != nil {
			return nil, err
		}
		for _, ix := range indices {
			span, ok := daySpan(ix.Name)
			if !ok || !span.Overlaps(r) {
				continue
			}
			p := estimate(ix.Name, span)
			if p.Stale {
				cp := *p
				cp.Rows, _ = strconv.ParseInt(ix.DocsCount, 10, 64)
				cp.Bytes, _ = strconv.ParseInt(ix.StoreSize, 10, 64)
				p = &cp
			}
			out = append(out, p)
		}
	}
	sortPartitions(out)
	return out, nil
}

func sortPartitions(p []*Partition) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Span.From.Before(p[j-1].Span.From); j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}
