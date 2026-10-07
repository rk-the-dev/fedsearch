// Command datagen produces the POC's tiered telemetry:
//
//	out/cold/<dataset>/dt=YYYY-MM-DD/part-00000.parquet   → MinIO (S3 archive)
//	out/hot/<dataset>/bulk-YYYY-MM-DD.ndjson              → OpenSearch (SIEM)
//	out/context/{hosts,ip_assignments}.csv                → Postgres (Reef stand-in)
//	out/ground_truth.json                                 → expected answers
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rksurwase/fedsearch/internal/datagen"
)

type tierCount struct {
	Events int `json:"events"`
	Files  int `json:"files"`
}

type groundTruth struct {
	Seed         int64                           `json:"seed"`
	Now          time.Time                       `json:"now"`
	Start        time.Time                       `json:"start"`
	HotFrom      time.Time                       `json:"hot_from"`
	ColdUntil    time.Time                       `json:"cold_until"`
	Counts       map[string]map[string]tierCount `json:"counts"`             // dataset → tier → count
	OverlapDupes map[string]int                  `json:"overlap_duplicates"` // dataset → events in both tiers
	Stages       []datagen.Stage                 `json:"attack_stages"`
	InjectionUA  string                          `json:"injection_user_agent"`
}

func main() {
	var (
		out       = flag.String("out", "out", "output directory")
		seed      = flag.Int64("seed", 42, "random seed (same seed + now = identical data)")
		nowStr    = flag.String("now", "", "anchor date YYYY-MM-DD (default: today UTC)")
		days      = flag.Int("days", 180, "days of history")
		users     = flag.Int("users", 200, "background users")
		authPerDy = flag.Int("auth-per-day", 4000, "background auth events per day")
		netPerDy  = flag.Int("net-per-day", 8000, "background network events per day")
		hotDays   = flag.Int("hot-days", 30, "SIEM retention: days kept in the hot tier")
		lagDays   = flag.Int("archive-lag-days", 23, "data older than this is in the cold tier")
	)
	flag.Parse()

	now := time.Now().UTC().Truncate(24 * time.Hour)
	if *nowStr != "" {
		t, err := time.Parse("2006-01-02", *nowStr)
		if err != nil {
			log.Fatalf("bad -now: %v", err)
		}
		now = t
	}
	if *lagDays >= *hotDays {
		log.Printf("warning: archive-lag-days >= hot-days → no overlap window; dedup won't be exercised")
	}
	if err := os.RemoveAll(*out); err != nil {
		log.Fatal(err)
	}

	r := rand.New(rand.NewSource(*seed))
	world := datagen.NewWorld(r, now, *days, *users)
	tiers := datagen.Tiers{Now: now, HotDays: *hotDays, ArchiveLagDays: *lagDays}

	var seq int64
	atkAuth, atkNet, stages := world.Attack(r, &seq)
	authByDay := groupByDay(atkAuth, func(e datagen.AuthEvent) time.Time { return e.Time })
	netByDay := groupByDay(atkNet, func(e datagen.NetEvent) time.Time { return e.Time })

	gt := groundTruth{
		Seed: *seed, Now: now, Start: world.Start,
		HotFrom: now.AddDate(0, 0, -*hotDays), ColdUntil: now.AddDate(0, 0, -*lagDays),
		Counts:       map[string]map[string]tierCount{"authentication": {}, "network_activity": {}},
		OverlapDupes: map[string]int{},
		Stages:       stages, InjectionUA: datagen.InjectionUA,
	}
	bump := func(ds, tier string, n int) {
		c := gt.Counts[ds][tier]
		c.Events += n
		c.Files++
		gt.Counts[ds][tier] = c
	}

	coldRoot, hotRoot := filepath.Join(*out, "cold"), filepath.Join(*out, "hot")
	started := time.Now()
	for day := world.Start; day.Before(now); day = day.AddDate(0, 0, 1) {
		auths, nets := world.Background(r, day, *authPerDy, *netPerDy, &seq)
		auths = append(auths, authByDay[day]...)
		nets = append(nets, netByDay[day]...)
		sort.Slice(auths, func(i, j int) bool { return auths[i].Time.Before(auths[j].Time) })
		sort.Slice(nets, func(i, j int) bool { return nets[i].Time.Before(nets[j].Time) })

		hot, cold := tiers.InHot(day), tiers.InCold(day)
		if cold {
			must(datagen.WriteParquet(coldRoot, "authentication", day, auths))
			must(datagen.WriteParquet(coldRoot, "network_activity", day, nets))
			bump("authentication", "cold", len(auths))
			bump("network_activity", "cold", len(nets))
		}
		if hot {
			must(datagen.WriteBulk(hotRoot, "authentication", "ocsf-authentication", day, auths))
			must(datagen.WriteBulk(hotRoot, "network_activity", "ocsf-network_activity", day, nets))
			bump("authentication", "hot", len(auths))
			bump("network_activity", "hot", len(nets))
		}
		if hot && cold {
			gt.OverlapDupes["authentication"] += len(auths)
			gt.OverlapDupes["network_activity"] += len(nets)
		}
	}
	if err := datagen.WriteContext(filepath.Join(*out, "context"), world); err != nil {
		log.Fatal(err)
	}
	b, _ := json.MarshalIndent(gt, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "ground_truth.json"), b, 0o644); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("generated %d days (%s → %s) in %s\n", *days, world.Start.Format("2006-01-02"), now.Format("2006-01-02"), time.Since(started).Round(time.Millisecond))
	fmt.Printf("  hot  (OpenSearch): %s onward\n", gt.HotFrom.Format("2006-01-02"))
	fmt.Printf("  cold (Parquet):    before %s\n", gt.ColdUntil.Format("2006-01-02"))
	for _, ds := range []string{"authentication", "network_activity"} {
		fmt.Printf("  %-17s hot=%d cold=%d overlap_dupes=%d\n", ds,
			gt.Counts[ds]["hot"].Events, gt.Counts[ds]["cold"].Events, gt.OverlapDupes[ds])
	}
	fmt.Printf("  hosts=%d ip_assignments=%d attack_stages=%d\n", len(world.Hosts), len(world.Assignments), len(stages))
}

func groupByDay[T any](rows []T, ts func(T) time.Time) map[time.Time][]T {
	m := map[time.Time][]T{}
	for _, r := range rows {
		d := ts(r).Truncate(24 * time.Hour)
		m[d] = append(m[d], r)
	}
	return m
}

func must(_ string, err error) {
	if err != nil {
		log.Fatal(err)
	}
}
