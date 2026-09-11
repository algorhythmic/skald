package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/algorhythmic/skald/sessioncapture"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
)

type DiscoveryRoot struct {
	Namespace     string    `json:"namespace"`
	Provider      string    `json:"provider"`
	Root          string    `json:"root"`
	RootAliases   []string  `json:"root_aliases,omitempty"`
	ModifiedSince time.Time `json:"modified_since"`
	MaxSessions   int       `json:"max_sessions"`
}
type Config struct {
	Discovery        []DiscoveryRoot        `json:"discovery,omitempty"`
	Version          int                    `json:"version"`
	PollMilliseconds int                    `json:"poll_interval_ms"`
	MaxDatabaseBytes int64                  `json:"max_database_bytes"`
	MinFreeBytes     uint64                 `json:"min_free_bytes"`
	AllowRaw         bool                   `json:"allow_raw"`
	Sources          []archive.Registration `json:"sources"`
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return cfg, err
	}
	if len(raw) > 1<<20 {
		return cfg, errors.New("configuration_too_large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, errors.New("invalid_configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("trailing_configuration_data")
	}
	if cfg.Version != 1 || cfg.PollMilliseconds < 100 || cfg.PollMilliseconds > 60000 || cfg.MaxDatabaseBytes < 1<<20 || cfg.MaxDatabaseBytes > 1<<50 || cfg.MinFreeBytes > 1<<50 || len(cfg.Sources) > 256 || len(cfg.Discovery) > 16 {
		return cfg, errors.New("invalid_configuration_limits")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, err
	}
	paths := map[string]bool{}
	streams := map[string]bool{}
	for i := range cfg.Sources {
		r := &cfg.Sources[i]
		if r.Root == "" {
			return cfg, errors.New("source_root_required")
		}
		r.Root = resolve(base, r.Root)
		for j, a := range r.RootAliases {
			r.RootAliases[j] = resolve(base, a)
		}
		if err := r.Validate(); err != nil {
			return cfg, err
		}
		path := filepath.Join(r.Root, r.Path)
		if paths[path] || streams[r.Key()] {
			return cfg, errors.New("duplicate_source")
		}
		paths[path] = true
		streams[r.Key()] = true
	}
	rootPaths := map[string]bool{}
	count := len(cfg.Sources)
	namespaces := map[string]bool{}
	for _, r := range cfg.Sources {
		namespaces[r.Namespace] = true
	}
	for i := range cfg.Discovery {
		r := &cfg.Discovery[i]
		if r.Namespace == "" || len(r.Namespace) > 256 || r.Root == "" || r.MaxSessions < 1 || r.MaxSessions > 256 || len(r.RootAliases) > 16 || (r.Provider != sessioncapture.Claude && r.Provider != sessioncapture.Codex && r.Provider != sessioncapture.Devin) {
			return cfg, errors.New("invalid_discovery_configuration")
		}
		if namespaces[r.Namespace] {
			return cfg, errors.New("discovery_namespace_must_be_exclusive")
		}
		namespaces[r.Namespace] = true
		r.Root = resolve(base, r.Root)
		locator := r.Provider + "\x00" + r.Root
		if rootPaths[locator] {
			return cfg, errors.New("duplicate_discovery_root")
		}
		rootPaths[locator] = true
		for j, a := range r.RootAliases {
			r.RootAliases[j] = resolve(base, a)
		}
		count += r.MaxSessions
	}
	if count > 256 {
		return cfg, errors.New("configured_source_capacity_exceeded")
	}

	return cfg, nil
}
func resolve(base, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}
func (c Config) Namespaces() []string {
	seen := map[string]bool{}
	for _, s := range c.Sources {
		seen[s.Namespace] = true
	}
	for _, r := range c.Discovery {
		seen[r.Namespace] = true
	}
	var ns []string
	for n := range seen {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}
func (c Config) Options() archive.Options {
	return archive.Options{MaxDatabaseBytes: c.MaxDatabaseBytes, MinFreeBytes: c.MinFreeBytes}
}

func DefaultDataDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("absolute_XDG_DATA_HOME_required")
	}
	return filepath.Join(base, "skald"), nil
}
func DefaultSocket() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" || !filepath.IsAbs(base) {
		return "", errors.New("set_socket_or_XDG_RUNTIME_DIR")
	}
	return filepath.Join(base, "skald", "skald.sock"), nil
}
