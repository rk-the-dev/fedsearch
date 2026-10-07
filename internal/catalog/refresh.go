package catalog

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/timex"
)

// Report summarizes one refresh for the CLI, the API and benchmarks.
type Report struct {
	StartedAt time.Time         `json:"started_at"`
	Duration  time.Duration     `json:"duration_ns"`
	Locations []*LocationReport `json:"locations"`
	Overlaps  []Overlap         `json:"overlaps"`
	Errors    []string          `json:"errors,omitempty"`
}

// Refresh discovers every configured location, reusing unchanged partitions
// from prev, and recomputes coverage, overlaps and field stats.
func Refresh(ctx context.Context, cfg *config.Config, prev *Catalog) (*Catalog, *Report, error) {
	rep := &Report{StartedAt: time.Now().UTC()}
	cat := &Catalog{Version: 1, RefreshedAt: rep.StartedAt, Datasets: NewDatasets()}
	if prev != nil {
		cat.Drift = prev.Drift
	}
	statsFrom := map[string]int{} // dataset -> preference of location stats came from

	for _, src := range cfg.Sources {
		for _, ds := range src.Datasets {
			dataset := cat.Datasets[ds]
			if dataset == nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("source %s: unknown dataset %q", src.Name, ds))
				continue
			}
			var prevLoc *Location
			if prev != nil {
				prevLoc = prev.Location(src.Name + "." + ds)
			}
			d, err := discovererFor(src.Kind)
			if err != nil {
				return nil, nil, err
			}
			loc, lrep, err := d.Discover(ctx, src, ds, prevLoc)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s.%s: %v", src.Name, ds, err))
				if prevLoc != nil { // keep the last known good location
					cat.Locations = append(cat.Locations, prevLoc)
				}
				continue
			}
			if err := requiredRoles(dataset, loc); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			if err := loc.Validate(dataset); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			cat.Drift = append(cat.Drift, lrep.Drift...)

			// Stats from the most preferred location (freshest data).
			if p, seen := statsFrom[ds]; !seen || src.Preference < p {
				rows, part, err := sampleLocation(ctx, src, loc)
				if err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: sampling: %v", loc.ID, err))
				} else if len(rows) > 0 {
					for path, st := range computeStats(dataset, loc, rows) {
						for _, f := range dataset.Fields {
							if f.Path == path {
								f.Stats = st
							}
						}
					}
					lrep.SampledFromPart = part
					statsFrom[ds] = src.Preference
				}
			}
			cat.Locations = append(cat.Locations, loc)
			rep.Locations = append(rep.Locations, lrep)
		}
	}
	sort.SliceStable(cat.Locations, func(i, j int) bool {
		if cat.Locations[i].Dataset != cat.Locations[j].Dataset {
			return cat.Locations[i].Dataset < cat.Locations[j].Dataset
		}
		return cat.Locations[i].Preference < cat.Locations[j].Preference
	})
	for _, ds := range KnownDatasets() {
		rep.Overlaps = append(rep.Overlaps, cat.Overlaps(ds)...)
	}
	rep.Duration = time.Since(rep.StartedAt)
	return cat, rep, nil
}

// Overlap is a time window that two locations of the same dataset both hold.
type Overlap struct {
	Dataset string      `json:"dataset"`
	A       string      `json:"a"`
	B       string      `json:"b"`
	Range   timex.Range `json:"range"`
}

// Overlaps lists every pair of locations whose stored data overlaps in time.
// Uses stored coverage (not live extension) because overlap is about data
// physically present in both places.
func (c *Catalog) Overlaps(dataset string) []Overlap {
	locs := c.LocationsFor(dataset)
	var out []Overlap
	for i := 0; i < len(locs); i++ {
		for j := i + 1; j < len(locs); j++ {
			if r, ok := locs[i].Coverage.Range.Intersect(locs[j].Coverage.Range); ok {
				out = append(out, Overlap{Dataset: dataset, A: locs[i].ID, B: locs[j].ID, Range: r})
			}
		}
	}
	return out
}
