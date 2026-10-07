// Package config describes where data lives and the policies that apply to
// it. One JSON file wires sources, the context store, budgets and the LLM.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	DataDir string         `json:"data_dir"` // where catalog.json, audit.jsonl and caches live
	Sources []Source       `json:"sources"`
	Context ContextStore   `json:"context"`
	Budget  Budget         `json:"budget"`
	LLM     LLM            `json:"llm"`
	Server  Server         `json:"server"`
	Agents  []AgentPrincip `json:"agents"`
}

// Source is one physical store holding one or more datasets.
type Source struct {
	Name       string     `json:"name"`       // "cold", "hot"; prefixes location IDs
	Kind       string     `json:"kind"`       // parquet | ndjson | opensearch
	Tier       string     `json:"tier"`       // hot | cold (presentation only)
	Preference int        `json:"preference"` // lower wins overlap ownership
	Live       bool       `json:"live"`       // still ingesting: coverage extends to now
	Root       string     `json:"root"`       // parquet/ndjson: dir path or s3://bucket/prefix
	S3         *S3        `json:"s3,omitempty"`
	URL        string     `json:"url,omitempty"` // opensearch
	IndexFmt   string     `json:"index_fmt,omitempty"`
	Datasets   []string   `json:"datasets"`
	Cost       CostConfig `json:"cost"`
	TimeoutSec int        `json:"timeout_sec"`
	MaxConc    int        `json:"max_concurrency"`
}

type S3 struct {
	Endpoint  string `json:"endpoint"` // host:port, e.g. localhost:9000
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	UseSSL    bool   `json:"use_ssl"`
}

type CostConfig struct {
	Kind string  `json:"kind"` // per_tb_scanned | per_gb_scanned | free
	Rate float64 `json:"rate"` // USD per unit
}

type ContextStore struct {
	Kind string `json:"kind"` // postgres | csv
	DSN  string `json:"dsn,omitempty"`
	Dir  string `json:"dir,omitempty"` // csv: directory with hosts.csv and ip_assignments.csv
}

type Budget struct {
	ConfirmAboveUSD float64 `json:"confirm_above_usd"`
	MaxPerQueryUSD  float64 `json:"max_per_query_usd"`
	PerHourUSD      float64 `json:"per_hour_usd"`
}

type LLM struct {
	Provider  string `json:"provider"` // anthropic | none
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
	BaseURL   string `json:"base_url"`
	Mode      string `json:"mode"` // llm_first | cache_first | cache_only
}

type Server struct {
	Addr string `json:"addr"`
}

// AgentPrincip is an MCP client identity with its own budget.
type AgentPrincip struct {
	Name         string   `json:"name"`
	APIKey       string   `json:"api_key"`
	PerHourUSD   float64  `json:"per_hour_usd"`
	AllowedTools []string `json:"allowed_tools"`
}

func (s Source) Timeout() time.Duration {
	if s.TimeoutSec <= 0 {
		if s.Tier == "cold" {
			return 60 * time.Second
		}
		return 5 * time.Second
	}
	return time.Duration(s.TimeoutSec) * time.Second
}

// Load reads a config file, expands ${ENV} references and resolves relative
// paths against the config file's directory.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(os.ExpandEnv(string(b)))
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	base := filepath.Dir(path)
	abs := func(p string) string {
		if p == "" || strings.HasPrefix(p, "s3://") || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.DataDir = abs(c.DataDir)
	if c.DataDir == "" {
		c.DataDir = filepath.Join(base, ".fedsearch")
	}
	for i := range c.Sources {
		c.Sources[i].Root = abs(c.Sources[i].Root)
	}
	c.Context.Dir = abs(c.Context.Dir)
	if c.Budget.MaxPerQueryUSD == 0 {
		c.Budget = Budget{ConfirmAboveUSD: 0.01, MaxPerQueryUSD: 1, PerHourUSD: 5}
	}
	if c.LLM.Mode == "" {
		c.LLM.Mode = "llm_first"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	names := map[string]bool{}
	for _, s := range c.Sources {
		if names[s.Name] {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		names[s.Name] = true
		switch s.Kind {
		case "parquet", "ndjson":
			if s.Root == "" {
				return fmt.Errorf("source %s: root is required", s.Name)
			}
		case "opensearch":
			if s.URL == "" {
				return fmt.Errorf("source %s: url is required", s.Name)
			}
		default:
			return fmt.Errorf("source %s: unknown kind %q", s.Name, s.Kind)
		}
	}
	return nil
}

// Source returns the named source.
func (c *Config) Source(name string) (Source, bool) {
	for _, s := range c.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
