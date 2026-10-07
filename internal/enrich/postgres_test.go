package enrich

import (
	"context"
	"os"
	"testing"
	"time"
)

// Runs against the Docker stack's Postgres (or any DB loaded with
// deploy/postgres/init.sql): FEDSEARCH_TEST_PG=postgres://... go test ./internal/enrich
func TestPostgresAsOf(t *testing.T) {
	dsn := os.Getenv("FEDSEARCH_TEST_PG")
	if dsn == "" {
		t.Skip("FEDSEARCH_TEST_PG not set")
	}
	st, err := Open(configFor(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := New(st)
	m, _ := LoadCSV("../../out/context")
	var churn time.Time
	for _, a := range m.byIP["10.20.4.17"] {
		if !a.To.IsZero() {
			churn = a.To
		}
	}
	c, _, err := e.Lookup(context.Background(), "10.20.4.17", churn.Add(-12*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.AsOf == nil || c.AsOf.Hostname != "lt-ankit-042" || c.Current == nil || c.Current.Hostname != "lt-meera-118" {
		t.Fatalf("context = %+v / %+v", c.AsOf, c.Current)
	}
}
