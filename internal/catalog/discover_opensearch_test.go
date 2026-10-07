package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rksurwase/fedsearch/internal/config"
)

func TestOpenSearchDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/_cat/indices/"):
			fmt.Fprint(w, `[{"index":"ocsf-authentication-2026.09.08","docs.count":"4000","store.size":"1200000"},
				{"index":"ocsf-authentication-2026.09.07","docs.count":"4041","store.size":"1210000"}]`)
		case strings.HasSuffix(r.URL.Path, "/_field_caps"):
			fmt.Fprint(w, `{"fields":{
				"time":{"date":{"type":"date","searchable":true,"aggregatable":true}},
				"event_id":{"keyword":{"type":"keyword","searchable":true,"aggregatable":true}},
				"status":{"keyword":{"type":"keyword","searchable":true,"aggregatable":true}},
				"user_name":{"keyword":{"type":"keyword","searchable":true,"aggregatable":true}},
				"src_endpoint_ip":{"ip":{"type":"ip","searchable":true,"aggregatable":true}},
				"http_user_agent":{"keyword":{"type":"keyword","searchable":false,"aggregatable":true}},
				"vendor_extra":{"keyword":{"type":"keyword","searchable":true,"aggregatable":true}},
				"_id":{"_id":{"type":"_id","searchable":true,"aggregatable":false}}}}`)
		case strings.HasSuffix(r.URL.Path, "/_search"):
			fmt.Fprint(w, `{"hits":{"total":{"value":0},"hits":[]},"aggregations":{"tmin":{"value":1788739200000},"tmax":{"value":1788911999000}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := config.Source{Name: "hot", Kind: "opensearch", Tier: "hot", Preference: 1, Live: true, URL: srv.URL}
	loc, rep, err := opensearchDiscoverer{}.Discover(context.Background(), src, "authentication", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(loc.Partitions) != 2 || loc.Partitions[0].Key != "ocsf-authentication-2026.09.07" || rep.Partitions != 2 {
		t.Fatalf("partitions: %+v", loc.Partitions)
	}
	if loc.Rows != 8041 || loc.AvgRowSize < 290 || loc.AvgRowSize > 310 {
		t.Fatalf("rows=%d avg=%f", loc.Rows, loc.AvgRowSize)
	}
	if b := loc.Bindings["http_request.user_agent"]; b == nil || b.Searchable {
		t.Fatalf("field_caps searchable flag lost: %+v", b)
	}
	if b := loc.Bindings["status_id"]; b == nil || b.Enum["failure"] != 2 {
		t.Fatalf("status binding: %+v", b)
	}
	if len(loc.Unmapped) != 1 || loc.Unmapped[0] != "vendor_extra" {
		t.Fatalf("unmapped = %v", loc.Unmapped)
	}
	if loc.Caps.Dialect != "opensearch_dsl" || !loc.Coverage.Live {
		t.Fatalf("caps/coverage: %+v %+v", loc.Caps, loc.Coverage)
	}
}
