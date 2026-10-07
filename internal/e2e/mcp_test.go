package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/rksurwase/fedsearch/internal/mcp"
	"github.com/rksurwase/fedsearch/internal/tools"
)

func TestMCPProtocol(t *testing.T) {
	s, _ := setup(t)
	srv := &mcp.Server{Hub: tools.NewHub(s, "mcp"), Principal: agentPrincipal(nil), Version: "test"}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_run","arguments":{"query":{"dataset":"network_activity","time":{"from":"now-180d"},"where":{"cmp":{"field":"dst_endpoint.ip","op":"eq","value":"185.220.101.47"}},"aggs":[{"fn":"count","as":"n"}]}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_run","arguments":{"query":{"dataset":"nope","time":{"from":"now-1d"}}}}}`,
	}, "\n")
	pr, pw := io.Pipe()
	go func() {
		_ = srv.Serve(context.Background(), strings.NewReader(in), pw)
		pw.Close()
	}()
	got := map[float64]map[string]any{}
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		got[m["id"].(float64)] = m
	}
	if len(got) != 4 {
		t.Fatalf("responses = %d, want 4 (notification gets none)", len(got))
	}
	if v := got[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("protocol = %v", v)
	}
	if n := len(got[2]["result"].(map[string]any)["tools"].([]any)); n != 4 {
		t.Fatalf("tools = %d", n)
	}
	call := got[3]["result"].(map[string]any)
	text := call["content"].([]any)[0].(map[string]any)["text"].(string)
	if call["isError"] != false || !strings.Contains(text, `"n": 30`) || !strings.Contains(text, "untrusted_data") {
		t.Fatalf("tool result: %s", text)
	}
	if bad := got[4]["result"].(map[string]any); bad["isError"] != true {
		t.Fatalf("invalid query should be a tool error: %v", bad)
	}
}
