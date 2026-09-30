// Package config loads and validates the Conduit YAML config.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	NodeID   string        `yaml:"node_id"`
	Listen   string        `yaml:"listen"`
	Token    string        `yaml:"token"`
	Database string        `yaml:"database"`
	Capture  CaptureConfig `yaml:"capture"`
	Peers    []Peer        `yaml:"peers"`

	// Sequences gives this node its own residue class for SERIAL/IDENTITY
	// ids (id % step == offset % step) so nodes never generate the same id.
	Sequences *SequenceConfig `yaml:"sequences"`
	// Tables holds per-table policies keyed by "schema.table" (or "table"
	// for the public schema).
	Tables map[string]TablePolicy `yaml:"tables"`
	// TombstoneTTL is how long deleted-row markers are kept for conflict
	// resolution. Must exceed the longest expected outage.
	TombstoneTTL time.Duration `yaml:"tombstone_ttl"`
}

type SequenceConfig struct {
	Offset int64 `yaml:"offset"`
	Step   int64 `yaml:"step"`
}

type TablePolicy struct {
	// Owner, when set, is the only node allowed to write this table.
	// Other nodes get a guard trigger and still receive its changes.
	Owner string `yaml:"owner"`
}

type CaptureConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Slot        string   `yaml:"slot"`
	Publication string   `yaml:"publication"`
	Schemas     []string `yaml:"schemas"`
}

type Peer struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// ValidID reports whether s is usable as a node id, slot or publication name.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Load reads a YAML config file, expanding ${ENV_VARS} first.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, cfg.validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":7420"
	}
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
	tables := make(map[string]TablePolicy, len(c.Tables))
	for name, p := range c.Tables {
		if !strings.Contains(name, ".") {
			name = "public." + name
		}
		tables[name] = p
	}
	c.Tables = tables
}

func (c *Config) validate() error {
	var errs []error
	if !ValidID(c.NodeID) {
		errs = append(errs, fmt.Errorf("node_id %q must match %s", c.NodeID, idPattern))
	}
	if c.Token == "" {
		errs = append(errs, errors.New("token is required"))
	}
	if c.Database == "" {
		errs = append(errs, errors.New("database is required"))
	}
	if !ValidID(c.Capture.Slot) || !ValidID(c.Capture.Publication) {
		errs = append(errs, errors.New("capture.slot and capture.publication must be simple lowercase names"))
	}
	if slices.Contains(c.Capture.Schemas, "conduit") {
		errs = append(errs, errors.New(`capture.schemas must not include "conduit" (it would replicate its own queue)`))
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		if !ValidID(p.ID) || p.URL == "" {
			errs = append(errs, fmt.Errorf("peer %q needs a valid id and url", p.ID))
		}
		if p.ID == c.NodeID || seen[p.ID] {
			errs = append(errs, fmt.Errorf("peer id %q is duplicated or equals node_id", p.ID))
		}
		seen[p.ID] = true
	}
	if sq := c.Sequences; sq != nil && (sq.Step < 1 || sq.Offset < 1 || sq.Offset > sq.Step) {
		errs = append(errs, errors.New("sequences needs 1 <= offset <= step"))
	}
	for name, p := range c.Tables {
		if p.Owner != "" && p.Owner != c.NodeID && !seen[p.Owner] {
			errs = append(errs, fmt.Errorf("tables.%s.owner %q is neither node_id nor a peer", name, p.Owner))
		}
	}
	return errors.Join(errs...)
}

// PeerIDs returns the ids of all configured peers.
func (c *Config) PeerIDs() []string {
	ids := make([]string, len(c.Peers))
	for i, p := range c.Peers {
		ids[i] = p.ID
	}
	return ids
}

// Owner returns the owning node of schema.table, or "" if any node may write.
func (c *Config) Owner(schema, table string) string {
	return c.Tables[schema+"."+table].Owner
}
