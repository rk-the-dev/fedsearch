package datagen

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"
)

// Tiers describes where data lands, mimicking a real deployment:
//   - hot  (SIEM stand-in, OpenSearch): the last HotDays days
//   - cold (S3 archive, Parquet): everything older than ArchiveLagDays
//
// When HotDays > ArchiveLagDays the window in between lives in BOTH tiers.
type Tiers struct {
	Now            time.Time
	HotDays        int
	ArchiveLagDays int
}

func (t Tiers) InHot(day time.Time) bool  { return !day.Before(t.Now.AddDate(0, 0, -t.HotDays)) }
func (t Tiers) InCold(day time.Time) bool { return day.Before(t.Now.AddDate(0, 0, -t.ArchiveLagDays)) }

// WriteParquet writes rows to <root>/<dataset>/dt=YYYY-MM-DD/part-00000.parquet.
// Rows must already be time-sorted: sorted files give tight min/max column
// statistics, which is what lets engines skip row groups later.
func WriteParquet[T any](root, dataset string, day time.Time, rows []T) (string, error) {
	dir := filepath.Join(root, dataset, "dt="+day.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "part-00000.parquet")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := parquet.NewGenericWriter[T](f, parquet.Compression(&parquet.Zstd), parquet.MaxRowsPerRowGroup(4096))
	if _, err := w.Write(rows); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// IDer is implemented by events so the bulk writer can use event_id as _id.
type IDer interface{ ID() string }

func (e AuthEvent) ID() string { return e.EventID }
func (e NetEvent) ID() string  { return e.EventID }

// WriteBulk writes an OpenSearch _bulk NDJSON file for one day, targeting a
// daily index (<prefix>-YYYY.MM.DD) the way SIEMs and log stores lay data out.
func WriteBulk[T IDer](root, dataset, indexPrefix string, day time.Time, rows []T) (string, error) {
	dir := filepath.Join(root, dataset)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "bulk-"+day.Format("2006-01-02")+".ndjson")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	index := fmt.Sprintf("%s-%s", indexPrefix, day.Format("2006.01.02"))
	for _, row := range rows {
		action := map[string]map[string]string{"index": {"_index": index, "_id": row.ID()}}
		if err := enc.Encode(action); err != nil {
			return "", err
		}
		if err := enc.Encode(row); err != nil {
			return "", err
		}
	}
	return path, bw.Flush()
}

// WriteContext writes the hosts and ip_assignments CSVs for Postgres COPY.
func WriteContext(root string, w *World) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	hosts := [][]string{{"hostname", "owner", "department", "kind", "criticality"}}
	for _, h := range w.Hosts {
		hosts = append(hosts, []string{h.Name, h.Owner, h.Department, h.Kind, h.Criticality})
	}
	if err := writeCSV(filepath.Join(root, "hosts.csv"), hosts); err != nil {
		return err
	}
	rows := [][]string{{"ip", "hostname", "valid_from", "valid_to"}}
	for _, a := range w.Assignments {
		to := "" // empty → NULL → open-ended (still assigned)
		if !a.To.IsZero() {
			to = a.To.Format(time.RFC3339)
		}
		rows = append(rows, []string{a.IP, a.Host, a.From.Format(time.RFC3339), to})
	}
	return writeCSV(filepath.Join(root, "ip_assignments.csv"), rows)
}

func writeCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cw := csv.NewWriter(f)
	if err := cw.WriteAll(rows); err != nil {
		return err
	}
	return cw.Error()
}
