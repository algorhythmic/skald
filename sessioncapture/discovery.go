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
	ConversationID  string `json:"conversation_id"`
	ProviderVersion string `json:"provider_version"`
	Project         string `json:"project,omitempty"`
	Originator      string `json:"originator,omitempty"`
	Evidence        string `json:"evidence"`
}

// Inventory walks an explicitly selected root through an os.Root. Symlinks and
// Claude subagent streams are excluded. An entry bound limits directory work;
// its failure is explicit rather than silently treating partial coverage as full.
func Inventory(ctx context.Context, root string, since time.Time, maxEntries int) ([]Candidate, error) {
	if maxEntries < 1 || maxEntries > 100000 {
		return nil, errors.New("invalid_discovery_limit")
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
			if e.Name() == "subagents" {
				return fs.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") || strings.HasPrefix(e.Name(), "agent-") {
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
	out := Identity{ProviderVersion: "unknown", Evidence: "bounded_native_prefix"}
	if provider != Claude && provider != Codex {
		return out, errors.New("unsupported_provider")
	}
	if !filepath.IsLocal(path) {
		return out, errors.New("invalid_source_path")
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
		if !complete {
			break
		}
		var native object
		if json.Unmarshal(line, &native) != nil || native == nil {
			continue
		}
		id := ""
		if provider == Claude {
			var sidechain bool
			_ = json.Unmarshal(native["isSidechain"], &sidechain)
			if sidechain || str(native, "agentId") != "" {
				return out, errors.New("subagent_identity_unsupported")
			}
			id = str(native, "sessionId")
			if v := str(native, "version"); v != "" {
				out.ProviderVersion = v
			}
			if v := str(native, "cwd"); v != "" {
				out.Project = v
			}
			kind := str(native, "type")
			shape = shape || ((kind == "user" || kind == "assistant") && obj(native, "message") != nil) || kind == "ai-title" || (kind == "system" && str(native, "subtype") == "away_summary")
		} else if str(native, "type") == "session_meta" {
			p := obj(native, "payload")
			id = str(p, "id")
			out.Project = str(p, "cwd")
			out.Originator = str(p, "originator")
			if v := str(p, "cli_version"); v != "" {
				out.ProviderVersion = v
			}
			shape = id != ""
		}
		if id != "" {
			if out.ConversationID != "" && out.ConversationID != id {
				return out, errors.New("conversation_identity_mismatch")
			}
			out.ConversationID = id
		}
	}
	after, err := dir.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return out, errors.New("source_replaced_during_read")
	}
	if out.ConversationID == "" || !shape {
		return out, errors.New("conversation_identity_unavailable")
	}
	if len(out.ConversationID) > 256 || len(out.ProviderVersion) > 128 || len(out.Project) > 4096 || len(out.Originator) > 256 {
		return out, errors.New("identity_metadata_too_large")
	}
	return out, nil
}
