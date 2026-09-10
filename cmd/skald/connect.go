package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/daemon"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

type sourcePreview struct {
	sessioncapture.Candidate
	Identity   *sessioncapture.Identity     `json:"identity,omitempty"`
	Capability *sessioncapture.Capabilities `json:"capabilities,omitempty"`
	Error      string                       `json:"error,omitempty"`
}

func defaultSourceRoot(provider string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch provider {
	case sessioncapture.Claude:
		base := os.Getenv("CLAUDE_CONFIG_DIR")
		if base == "" {
			base = filepath.Join(home, ".claude")
		}
		return filepath.Join(base, "projects"), nil
	case sessioncapture.Codex:
		base := os.Getenv("CODEX_HOME")
		if base == "" {
			base = filepath.Join(home, ".codex")
		}
		return filepath.Join(base, "sessions"), nil
	}
	return "", errors.New("unsupported_provider")
}
func runConnect(args []string, out, stderr io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	provider := f.String("provider", "", "claude_code or codex")
	root := f.String("root", "", "explicit source root; defaults to the provider's local conversation directory")
	config := f.String("config", "", "source configuration; defaults to the private Skald config directory")
	namespace := f.String("namespace", "", "stable namespace override (persisted once)")
	since := f.String("since", "", "include files modified since RFC3339 timestamp or all; new connections default to seven days ago")
	limit := f.Int("limit", 10, "maximum source metadata previews (1–64)")
	capacity := f.Int("max-sessions", 64, "maximum enrolled files for this root (total configured capacity ≤256)")
	previous := f.String("previous-root", "", "explicit former root for an intentional relocation; requires --namespace")
	if err := f.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	if *provider != sessioncapture.Claude && *provider != sessioncapture.Codex {
		return errors.New("supported_provider_required")
	}
	visited := map[string]bool{}
	f.Visit(func(v *flag.Flag) { visited[v.Name] = true })
	if args[0] == "sources" && (visited["config"] || visited["namespace"] || visited["max-sessions"] || visited["previous-root"]) {
		return errors.New("flag_not_supported_for_command")
	}
	if args[0] == "connect" && visited["limit"] {
		return errors.New("flag_not_supported_for_command")
	}
	if *limit < 1 || *limit > 64 {
		return errors.New("invalid_preview_limit")
	}
	if *root == "" {
		p, err := defaultSourceRoot(*provider)
		if err != nil {
			return err
		}
		*root = p
	}
	canonical, err := filepath.EvalSymlinks(*root)
	if err != nil {
		return errors.New("source_root_unavailable")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return errors.New("source_directory_required")
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if *since == "all" {
		cutoff = time.Time{}
	} else if *since != "" {
		cutoff, err = time.Parse(time.RFC3339, *since)
		if err != nil {
			return errors.New("since_requires_RFC3339_or_all")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "sources" {
		candidates, err := sessioncapture.Inventory(ctx, canonical, cutoff, 100000)
		if err != nil {
			return err
		}
		var sourceBytes int64
		for _, candidate := range candidates {
			sourceBytes += candidate.Bytes
		}
		items := []sourcePreview{}
		for _, c := range candidates[:min(*limit, len(candidates))] {
			item := sourcePreview{Candidate: c}
			identity, err := sessioncapture.Identify(ctx, canonical, c.Path, *provider)
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Identity = &identity
				capability := sessioncapture.Probe(*provider, identity.ProviderVersion)
				item.Capability = &capability
			}
			items = append(items, item)
		}
		return writeJSON(out, map[string]any{"provider": *provider, "root": canonical, "modified_since": cutoff, "candidates": len(candidates), "source_bytes": sourceBytes, "items": items, "preview_only": true, "coverage": "bounded metadata prefix; no conversation text emitted"})
	}
	if *config == "" {
		p, err := daemon.DefaultConfigPath()
		if err != nil {
			return err
		}
		*config = p
	}
	*config, err = filepath.Abs(*config)
	if err != nil {
		return err
	}
	if *previous != "" && *namespace == "" {
		return errors.New("relocation_requires_namespace")
	}
	result := daemon.DiscoveryRoot{Namespace: *namespace, Provider: *provider, Root: canonical, ModifiedSince: cutoff, MaxSessions: *capacity}
	if *previous != "" {
		alias, err := filepath.Abs(*previous)
		if err != nil {
			return err
		}
		result.RootAliases = []string{filepath.Clean(alias)}
	}
	err = daemon.UpdateConfig(*config, func(cfg *daemon.Config) error {
		for i, existing := range cfg.Discovery {
			sameRoot := existing.Provider == result.Provider && existing.Root == result.Root
			sameNamespace := result.Namespace != "" && existing.Namespace == result.Namespace
			if !sameRoot && !sameNamespace {
				continue
			}
			if sameNamespace && existing.Provider != result.Provider {
				return errors.New("namespace_provider_conflict")
			}
			if sameRoot && result.Namespace != "" && existing.Namespace != result.Namespace {
				return errors.New("root_already_connected_with_different_namespace")
			}
			if existing.Root != result.Root && (*previous == "" || result.RootAliases[0] != existing.Root) {
				return errors.New("root_relocation_requires_explicit_alias")
			}
			result.Namespace = existing.Namespace
			if !visited["since"] {
				result.ModifiedSince = existing.ModifiedSince
			}
			if !visited["max-sessions"] {
				result.MaxSessions = existing.MaxSessions
			}
			aliases := append(append([]string{}, existing.RootAliases...), result.RootAliases...)
			result.RootAliases = nil
			seen := map[string]bool{}
			for _, alias := range aliases {
				if !seen[alias] {
					result.RootAliases = append(result.RootAliases, alias)
					seen[alias] = true
				}
			}
			cfg.Discovery[i] = result
			return nil
		}
		if result.Namespace == "" {
			host, err := os.ReadFile("/etc/machine-id")
			if err != nil || strings.TrimSpace(string(host)) == "" {
				return errors.New("stable_host_identity_unavailable; specify_namespace")
			}
			result.Namespace, err = sessionrecord.Namespace(result.Provider, strings.TrimSpace(string(host)), result.Root, "", "")
			if err != nil {
				return err
			}
		}
		cfg.Discovery = append(cfg.Discovery, result)
		return nil
	})
	if err != nil {
		return err
	}
	return writeJSON(out, map[string]any{"configured": true, "collecting": false, "config": *config, "source": result, "next": "start or restart skald serve with this configuration; source files are read-only"})
}
