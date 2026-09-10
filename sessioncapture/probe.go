package sessioncapture

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type Capability struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}
type Capabilities struct {
	Provider          string     `json:"provider"`
	ProviderVersion   string     `json:"provider_version"`
	AdapterVersion    string     `json:"adapter_version"`
	Evidence          string     `json:"evidence"`
	OrdinaryContent   Capability `json:"ordinary_content"`
	NativeRecap       Capability `json:"native_recap"`
	LiveCollection    Capability `json:"live_collection"`
	SurfaceActivation Capability `json:"surface_activation"`
}

func Probe(provider, version string) Capabilities {
	if version == "" {
		version = "unknown"
	}
	unknown := Capability{State: "unknown", Reason: "provider_version_not_fixture_verified"}
	c := Capabilities{Provider: provider, ProviderVersion: version, AdapterVersion: AdapterVersion,
		Evidence:        "synthetic schema fixtures; September 9 source-shape audit",
		OrdinaryContent: unknown, NativeRecap: unknown,
		LiveCollection:    Capability{State: "unknown", Reason: "configured_file_collector_requires_source_verification"},
		SurfaceActivation: Capability{State: "unsupported", Reason: "surface_adapter_not_implemented"}}
	if provider == Claude && version == "2.1.263" {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "claude_message_blocks"}
		c.NativeRecap = Capability{State: "fixture_verified", Reason: "system_away_summary_content"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "configured_file_tail_and_restart_tests"}
	}
	if provider == Codex && version == "0.153.4" {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "codex_response_item"}
		c.NativeRecap = Capability{State: "unsupported", Reason: "native_tui_recap_not_persisted"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "configured_file_tail_and_restart_tests"}
	}
	if provider != Claude && provider != Codex {
		c.OrdinaryContent = Capability{State: "unsupported", Reason: "unsupported_provider"}
		c.NativeRecap = c.OrdinaryContent
		c.LiveCollection = c.OrdinaryContent
	}
	return c
}

// Discover inventories only an explicitly configured root. Symlinks are not
// traversed, and hitting the file bound is an error, not silent incomplete coverage.
func Discover(root string, limit int) ([]string, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("invalid_discovery_limit")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(canonical, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&fs.ModeSymlink != 0 || e.IsDir() {
			return nil
		}
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
			if len(paths) == limit {
				return errors.New("discovery_limit_exceeded")
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}
