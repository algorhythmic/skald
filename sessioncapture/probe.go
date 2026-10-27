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
	NativeSubagents   Capability `json:"native_subagents"`
	NativeForks       Capability `json:"native_forks"`
	ConversationAlias Capability `json:"conversation_alias"`
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
		NativeSubagents:   Capability{State: "unsupported", Reason: "subagent_identity_contract_not_implemented"},
		NativeForks:       Capability{State: "unsupported", Reason: "fork_relation_contract_not_implemented"},
		ConversationAlias: Capability{State: "unsupported", Reason: "conversation_alias_contract_not_implemented"},
		LiveCollection:    Capability{State: "unknown", Reason: "configured_file_collector_requires_source_verification"},
		SurfaceActivation: Capability{State: "unsupported", Reason: "surface_adapter_not_implemented"}}
	if provider == Claude && (version == "2.1.263" || version == "2.1.296" || version == "2.1.303") {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "claude_message_blocks"}
		c.NativeRecap = Capability{State: "fixture_verified", Reason: "system_away_summary_content"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "configured_file_tail_and_restart_tests"}
	}
	if provider == Claude && (version == "2.1.303" || version == "2.1.296") {
		c.NativeSubagents = Capability{State: "fixture_verified", Reason: "native_sidechain_agent_session_identity_and_child_capture"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "current_version_checkpoint_serialization_and_restart_tests"}
		c.Evidence = "synthetic current-version message/away-summary/restart fixtures; October 22 metadata-only schema audit"
	}
	if provider == Codex && version == "0.153.4" {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "codex_response_item"}
		c.NativeRecap = Capability{State: "unsupported", Reason: "native_tui_recap_not_persisted"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "configured_file_tail_and_restart_tests"}
		c.NativeForks = Capability{State: "fixture_verified", Reason: "session_meta_parent_extension; relationship_projection_unavailable"}
	}
	if provider == Codex && version == "0.163.0" {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "codex_0163_response_item_input_text_output_text"}
		c.NativeRecap = Capability{State: "unsupported", Reason: "native_tui_recap_not_persisted"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "current_version_checkpoint_serialization_and_restart_tests"}
		c.NativeForks = Capability{State: "fixture_verified", Reason: "bounded_forked_from_id_lineage_child_scope_and_parent_relation"}
		c.Evidence = "synthetic current-version response-item/restart and fork fixtures; October 22 metadata-only schema audit"
	}
	if provider == Devin {
		c.OrdinaryContent = Capability{State: "fixture_verified", Reason: "acp_messages_table"}
		c.NativeRecap = Capability{State: "unsupported", Reason: "no_native_recap_in_store"}
		c.LiveCollection = Capability{State: "fixture_verified", Reason: "sqlite_snapshot_dump"}
	}
	if provider != Claude && provider != Codex && provider != Devin {
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
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if e.IsDir() {
			if e.Name() == "subagents" {
				return fs.SkipDir
			}
			return nil
		}
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasPrefix(e.Name(), "agent-") {
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
