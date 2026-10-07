package objstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rksurwase/fedsearch/internal/config"
)

// fakeS3 implements the two S3 calls the store uses (ListObjectsV2 with and
// without a delimiter, ranged GetObject) over a local directory, path-style,
// the way MinIO serves them.
func fakeS3(t *testing.T, bucket, base, dir string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		if parts[0] != bucket {
			http.Error(w, "no bucket", 404)
			return
		}
		if len(parts) == 1 || parts[1] == "" { // ListObjectsV2
			prefix, delim := r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter")
			type obj struct {
				Key  string
				Size int64
				ETag string
			}
			type cp struct{ Prefix string }
			var res struct {
				XMLName        xml.Name `xml:"ListBucketResult"`
				Contents       []obj
				CommonPrefixes []cp
				IsTruncated    bool
			}
			seen := map[string]bool{}
			_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(dir, p)
				key := base + "/" + filepath.ToSlash(rel)
				if !strings.HasPrefix(key, prefix) {
					return nil
				}
				if delim != "" {
					rest := strings.TrimPrefix(key, prefix)
					if i := strings.Index(rest, delim); i >= 0 {
						if c := prefix + rest[:i+1]; !seen[c] {
							seen[c] = true
							res.CommonPrefixes = append(res.CommonPrefixes, cp{c})
						}
						return nil
					}
				}
				res.Contents = append(res.Contents, obj{key, info.Size(), fmt.Sprintf(`"%d"`, info.ModTime().UnixNano())})
				return nil
			})
			sort.Slice(res.CommonPrefixes, func(i, j int) bool { return res.CommonPrefixes[i].Prefix < res.CommonPrefixes[j].Prefix })
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(res)
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(parts[1], base+"/"))))
		if err != nil {
			http.Error(w, "NoSuchKey", 404)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			var start, end int
			fmt.Sscanf(strings.TrimPrefix(rg, "bytes="), "%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(b)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(206)
			_, _ = w.Write(b[start : end+1])
			return
		}
		_, _ = w.Write(b)
	}))
}

func TestS3StoreAgainstFake(t *testing.T) {
	cold, _ := filepath.Abs("../../out/cold")
	if _, err := os.Stat(cold); err != nil {
		t.Skip("no generated data; run `make gen`")
	}
	// Bucket "telemetry-archive" with the generated tree under prefix "ocsf".
	srv := fakeS3(t, "telemetry-archive", "ocsf", cold)
	defer srv.Close()
	st, err := Open("s3://telemetry-archive/ocsf", &config.S3{Endpoint: strings.TrimPrefix(srv.URL, "http://"), AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dirs, err := st.Dirs(ctx, "authentication")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 100 || !strings.HasPrefix(dirs[0], "authentication/dt=") {
		t.Fatalf("dirs = %d, first %q", len(dirs), dirs[0])
	}
	objs, err := st.List(ctx, dirs[0])
	if err != nil || len(objs) != 1 || !strings.HasSuffix(objs[0].Key, ".parquet") {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	r, err := st.ReaderAt(ctx, objs[0].Key, objs[0].Size)
	if err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, 4)
	if _, err := r.ReadAt(tail, objs[0].Size-4); err != nil || string(tail) != "PAR1" {
		t.Fatalf("ranged read = %q, %v", tail, err)
	}
	if r.BytesRead() != 4 {
		t.Fatalf("bytes read = %d", r.BytesRead())
	}
	if u := st.URI(objs[0].Key); !strings.HasPrefix(u, "s3://telemetry-archive/ocsf/authentication/dt=") {
		t.Fatalf("uri = %s", u)
	}
}
