package sessioncapture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Candidate contains metadata only. Discovery does not emit conversation bodies.
type Candidate struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified_at"`
}
type Identity struct {
	ConversationID       string `json:"conversation_id"`
	ProviderVersion      string `json:"provider_version"`
	Project              string `json:"project,omitempty"`
	Originator           string `json:"originator,omitempty"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	NativeAgentID        string `json:"native_agent_id,omitempty"`
	Evidence             string `json:"evidence"`
}

type InventoryOptions struct {
	IncludeClaudeSubagents bool
}

// Inventory walks an explicitly selected root through an os.Root. Symlinks and
// Claude subagent streams are excluded. An entry bound limits directory work;
// its failure is explicit rather than silently treating partial coverage as full.
func candidateFile(name, provider string) bool {
	if provider == Devin {
		return strings.HasSuffix(name, ".db")
	}
	return strings.HasSuffix(name, ".jsonl")
}

// These locators enter Inventory only through the Claude child opt-in. Their
// native content must prove a child even if they happen to contain a valid
// primary-shaped message. A relocated child may use an ordinary filename.
func claudeChildOnlyPath(path string) bool {
	if strings.HasPrefix(filepath.Base(path), "agent-") {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		if part == "subagents" {
			return true
		}
	}
	return false
}

func Inventory(ctx context.Context, root, provider string, since time.Time, maxEntries int) ([]Candidate, error) {
	return InventoryWithOptions(ctx, root, provider, since, maxEntries, InventoryOptions{})
}

// InventoryWithOptions includes Claude's native child streams only when the
// caller opts in. Candidate paths remain untrusted until Identify verifies the
// native parent session ID, agent ID and sidechain marker in a bounded prefix.
func InventoryWithOptions(ctx context.Context, root, provider string, since time.Time, maxEntries int, options InventoryOptions) ([]Candidate, error) {
	if maxEntries < 1 || maxEntries > 100000 {
		return nil, errors.New("invalid_discovery_limit")
	}
	if options.IncludeClaudeSubagents && provider != Claude {
		return nil, errors.New("unsupported_discovery_option")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("discovery_root_unavailable")
	}
	defer dir.Close()
	out := []Candidate{}
	seen := 0
	err = fs.WalkDir(dir.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.New("discovery_entry_unavailable")
		}
		seen++
		if seen > maxEntries {
			return errors.New("discovery_limit_exceeded")
		}
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if e.IsDir() {
			if e.Name() == "subagents" && !options.IncludeClaudeSubagents {
				return fs.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() || !candidateFile(e.Name(), provider) || (strings.HasPrefix(e.Name(), "agent-") && !options.IncludeClaudeSubagents) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return errors.New("discovery_entry_unavailable")
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			return nil
		}
		out = append(out, Candidate{Path: filepath.FromSlash(path), Bytes: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.After(out[j].Modified)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Identify verifies native identity and provider shape in a bounded prefix. The
// collector revalidates every exposed native ID while ingesting the whole file.
// A metadata-only prefix before Claude's first message is allowed; identity never
// comes from a filename, title, cwd or observation time.
func Identify(ctx context.Context, root, path, provider string) (Identity, error) {
	return IdentifyWithOptions(ctx, root, path, provider, InventoryOptions{})
}

// IdentifyWithOptions keeps child identity enrollment explicit. The option
// permits native child evidence; it never infers identity from the locator.
func IdentifyWithOptions(ctx context.Context, root, path, provider string, options InventoryOptions) (Identity, error) {
	out := Identity{ProviderVersion: "unknown", Evidence: "bounded_native_prefix"}
	if provider != Claude && provider != Codex && provider != Devin {
		return out, errors.New("unsupported_provider")
	}
	if options.IncludeClaudeSubagents && provider != Claude {
		return out, errors.New("unsupported_discovery_option")
	}
	if !filepath.IsLocal(path) {
		return out, errors.New("invalid_source_path")
	}
	if provider == Devin {
		return identifyDevin(ctx, root, path)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return out, errors.New("discovery_root_unavailable")
	}
	defer dir.Close()
	f, err := dir.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, errors.New("source_unavailable")
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return out, errors.New("regular_source_required")
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, 4<<20), 64<<10)
	shape := false
	childShape := false
	var claudeMode string
	var controlParent, controlAgent string
	var codexContext codexForkContext
	probeBytes := int64(0)
	probeRecords := 0
	for n := 0; n < 256; n++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		line, complete, oversized, err := readLine(reader, 4<<20)
		if err != nil {
			return out, errors.New("source_unavailable")
		}
		if oversized {
			return out, errors.New("identity_probe_limit")
		}
		probeBytes += int64(len(line))
		if !complete {
			break
		}
		probeRecords++
		var native object
		if json.Unmarshal(line, &native) != nil || native == nil {
			continue
		}
		id := ""
		if provider == Claude {
			nativeID, err := claudeIdentity(native)
			if err != nil {
				return out, err
			}
			if nativeID.known {
				mode := "primary"
				if nativeID.child {
					if !options.IncludeClaudeSubagents {
						return out, errors.New("subagent_identity_unsupported")
					}
					mode = "child"
				}
				if claudeMode != "" && claudeMode != mode {
					return out, errors.New("conversation_identity_mismatch")
				}
				claudeMode = mode
			}
			if nativeID.control {
				if (controlAgent != "" && controlAgent != nativeID.agent) || (nativeID.parent != "" && controlParent != "" && controlParent != nativeID.parent) || (claudeMode == "child" && (nativeID.agent != out.NativeAgentID || (nativeID.parent != "" && nativeID.parent != out.ParentConversationID))) {
					return out, errors.New("conversation_identity_mismatch")
				}
				controlAgent = nativeID.agent
				if nativeID.parent != "" {
					controlParent = nativeID.parent
				}
			}
			id = nativeID.conversationID()
			if nativeID.control {
				id = ""
			}
			if nativeID.child {
				if (controlAgent != "" && controlAgent != nativeID.agent) || (controlParent != "" && controlParent != nativeID.parent) {
					return out, errors.New("conversation_identity_mismatch")
				}
				out.ParentConversationID = nativeID.parent
				out.NativeAgentID = nativeID.agent
			}
			if v := str(native, "version"); v != "" {
				out.ProviderVersion = v
			}
			if v := str(native, "cwd"); v != "" {
				out.Project = v
			}
			kind := str(native, "type")
			shape = shape || ((kind == "user" || kind == "assistant") && obj(native, "message") != nil) || kind == "ai-title" || (kind == "system" && str(native, "subtype") == "away_summary") || (nativeID.child && kind == "attachment")
			childShape = childShape || (nativeID.child && (((kind == "user" || kind == "assistant") && obj(native, "message") != nil) || kind == "attachment"))
		} else if str(native, "type") == "session_meta" {
			p := obj(native, "payload")
			metadata, err := codexSessionMetadata(native)
			if err != nil {
				return out, err
			}
			if err := codexContext.observe(metadata); err != nil {
				return out, err
			}
			id = codexContext.root
			// Copied ancestor headers prove lineage, not the child's project,
			// originator, version, or immediate parent.
			if metadata.id != "" && metadata.id == codexContext.root {
				out.Project = str(p, "cwd")
				out.Originator = str(p, "originator")
				out.ParentConversationID = metadata.parent
				if v := str(p, "cli_version"); v != "" {
					out.ProviderVersion = v
				}
			}
			shape = shape || id != ""
		}
		if id != "" {
			if out.ConversationID != "" && out.ConversationID != id {
				return out, errors.New("conversation_identity_mismatch")
			}
			out.ConversationID = id
		}
	}
	after, err := dir.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !sameChangeTime(before, after) {
		return out, errors.New("source_replaced_during_read")
	}
	if provider == Claude && controlAgent != "" && (!options.IncludeClaudeSubagents || claudeMode != "child") {
		return out, errors.New("subagent_identity_unsupported")
	}
	if provider == Claude && claudeChildOnlyPath(path) && claudeMode == "primary" {
		return out, errors.New("subagent_identity_unsupported")
	}
	if out.ConversationID == "" || !shape || (claudeMode == "child" && !childShape) {
		if before.Size() > probeBytes && (probeRecords == 256 || probeBytes >= 4<<20) {
			return out, errors.New("identity_probe_limit")
		}
		return out, errors.New("conversation_identity_unavailable")
	}
	if len(out.ConversationID) > 256 || len(out.ProviderVersion) > 128 || len(out.Project) > 4096 || len(out.Originator) > 256 || len(out.ParentConversationID) > 256 || len(out.NativeAgentID) > 256 {
		return out, errors.New("identity_metadata_too_large")
	}
	return out, nil
}

func sameChangeTime(a, b os.FileInfo) bool {
	sa, aok := a.Sys().(*syscall.Stat_t)
	sb, bok := b.Sys().(*syscall.Stat_t)
	return aok && bok && sa.Ctim == sb.Ctim
}
