// Command mcp exposes FedSearch's governed tools to MCP clients over stdio.
//
//	FEDSEARCH_AGENT_KEY=agent-dev-key fedsearch-mcp -config deploy/lite.json
//
// Example Claude Desktop / Claude Code config:
//
//	{"mcpServers": {"fedsearch": {"command": "/path/to/fedsearch-mcp",
//	  "args": ["-config", "/path/to/deploy/lite.json"],
//	  "env": {"FEDSEARCH_AGENT_KEY": "agent-dev-key"}}}}
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/mcp"
	"github.com/rksurwase/fedsearch/internal/service"
	"github.com/rksurwase/fedsearch/internal/tools"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "deploy/lite.json", "config file")
	flag.Parse()
	log.SetOutput(os.Stderr) // stdout carries the protocol
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	p, err := tools.Authenticate(cfg, os.Getenv("FEDSEARCH_AGENT_KEY"))
	if err != nil {
		log.Fatalf("%v (set FEDSEARCH_AGENT_KEY to a key from the config's agents list)", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	svc, err := service.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	srv := &mcp.Server{Hub: tools.NewHub(svc, "mcp"), Principal: p, Version: version}
	log.Printf("fedsearch MCP server ready for principal %s", p.Name)
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
