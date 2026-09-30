// Package config loads Conduit settings from an optional YAML file and
// CONDUIT_* environment variables (env wins). Everything has a default
// except the database URL, so a new site needs almost nothing.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	NodeID   string `yaml:"node_id"`
	Listen   string `yaml:"listen"`
	Database string `yaml:"database"`
	// Advertise is the URL other sites use to reach this one.
	Advertise string `yaml:"advertise"`

	// Joining. A new site either founds the cluster (Bootstrap), redeems an
	// invite code (Join), or waits to be discovered and approved on the LAN.
	Bootstrap bool   `yaml:"bootstrap"`
	Join      string `yaml:"join"`
	Discovery bool   `yaml:"discovery"`
	// Token optionally fixes the cluster secret when founding. Normally it is
	// generated and handed to new sites inside the invite.
	Token string `yaml:"token"`
	// AdminPassword unlocks dashboard actions (invite, approve, remove).
	AdminPassword string `yaml:"admin_password"`

	Capture CaptureConfig `yaml:"capture"`

	// Tables holds per-table policies keyed by "schema.table". Set on any
	// site and it is shared with the whole cluster.
	Tables map[string]TablePolicy `yaml:"tables"`

	TombstoneTTL time.Duration `yaml:"tombstone_ttl"`
	// OutboxRetention keeps delivered changes a while so a site that is
	// joining right now cannot miss any.
	OutboxRetention time.Duration `yaml:"outbox_retention"`

	// Set at runtime once the site has an identity in the cluster.
	Sequences *SequenceConfig `yaml:"-"`

	mu sync.RWMutex
}

type SequenceConfig struct {
	Offset int64 `json:"offset"`
	Step   int64 `json:"step"`
}

type TablePolicy struct {
	// Owner, when set, is the only site allowed to write this table.
	Owner string `yaml:"owner" json:"owner"`
}

type CaptureConfig struct {
	Slot        string   `yaml:"slot"`
	Publication string   `yaml:"publication"`
	Schemas     []string `yaml:"schemas"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// ValidID reports whether s is usable as a node id, slot or publication name.
func ValidID(s string) bool { return idPattern.MatchString(s) }

var invalidIDChars = regexp.MustCompile(`[^a-z0-9_]+`)

// SanitizeID turns an arbitrary name (e.g. a hostname) into a valid id.
func SanitizeID(s string) string {
	s = strings.Trim(invalidIDChars.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "n" + s
	}
	if len(s) > 31 {
		s = s[:31]
	}
	return s
}

// Load reads path if it exists (a missing file is fine), then applies env.
func Load(path string) (*Config, error) {
	cfg := &Config{Discovery: true}
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cfg.applyEnv()
	cfg.applyDefaults()
	return cfg, cfg.validate()
}

func (c *Config) applyEnv() {
	str := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v := os.Getenv(key); v != "" {
			*dst = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		}
	}
	str("CONDUIT_NODE_ID", &c.NodeID)
	str("CONDUIT_LISTEN", &c.Listen)
	str("CONDUIT_DATABASE", &c.Database)
	str("CONDUIT_ADVERTISE", &c.Advertise)
	str("CONDUIT_JOIN", &c.Join)
	str("CONDUIT_TOKEN", &c.Token)
	str("CONDUIT_ADMIN_PASSWORD", &c.AdminPassword)
	boolean("CONDUIT_BOOTSTRAP", &c.Bootstrap)
	boolean("CONDUIT_DISCOVERY", &c.Discovery)
}

func (c *Config) applyDefaults() {
	host, _ := os.Hostname()
	if c.Listen == "" {
		c.Listen = ":7420"
	}
	if c.NodeID == "" {
		c.NodeID = SanitizeID(host)
	}
	if c.Advertise == "" {
		_, port, _ := net.SplitHostPort(c.Listen)
		c.Advertise = "http://" + host + ":" + port
	}
	c.Advertise = strings.TrimRight(c.Advertise, "/")
	if c.Capture.Slot == "" {
		c.Capture.Slot = "conduit_slot"
	}
	if c.Capture.Publication == "" {
		c.Capture.Publication = "conduit_pub"
	}
	if len(c.Capture.Schemas) == 0 {
		c.Capture.Schemas = []string{"public"}
	}
	if c.TombstoneTTL == 0 {
		c.TombstoneTTL = 7 * 24 * time.Hour
	}
	if c.OutboxRetention == 0 {
		c.OutboxRetention = time.Hour
	}
	c.Tables = normalizeTables(c.Tables)
}

func normalizeTables(in map[string]TablePolicy) map[string]TablePolicy {
	out := make(map[string]TablePolicy, len(in))
	for name, p := range in {
		if !strings.Contains(name, ".") {
			name = "public." + name
		}
		out[name] = p
	}
	return out
}

func (c *Config) validate() error {
	var errs []error
	if !ValidID(c.NodeID) {
		errs = append(errs, fmt.Errorf("node_id %q must match %s", c.NodeID, idPattern))
	}
	if c.Database == "" {
		errs = append(errs, errors.New("database is required (CONDUIT_DATABASE)"))
	}
	if !ValidID(c.Capture.Slot) || !ValidID(c.Capture.Publication) {
		errs = append(errs, errors.New("capture.slot and capture.publication must be simple lowercase names"))
	}
	if slices.Contains(c.Capture.Schemas, "conduit") {
		errs = append(errs, errors.New(`capture.schemas must not include "conduit" (it would replicate its own queue)`))
	}
	if c.Bootstrap && c.Join != "" {
		errs = append(errs, errors.New("set either bootstrap or join, not both"))
	}
	return errors.Join(errs...)
}

// Owner returns the owning site of schema.table, or "" if any site may write.
func (c *Config) Owner(schema, table string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Tables[schema+"."+table].Owner
}

// TablesCopy returns the current table policies.
func (c *Config) TablesCopy() map[string]TablePolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]TablePolicy, len(c.Tables))
	for k, v := range c.Tables {
		out[k] = v
	}
	return out
}

// SetTables replaces the table policies (they arrive via the cluster).
func (c *Config) SetTables(t map[string]TablePolicy) {
	t = normalizeTables(t)
	c.mu.Lock()
	c.Tables = t
	c.mu.Unlock()
}
