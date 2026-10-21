package sessioncapture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/algorhythmic/skald/sessionrecord"
	_ "modernc.org/sqlite"
)

// Devin desktop stores each ACP session as a private SQLite database: a meta
// table (session info, including the native title) and an append-only messages
// table (position, kind, payload). The store has no line stream, so capture
// canonicalizes committed rows to JSONL — each line embeds the exact stored
// payload — and the shared reader then tracks offsets, generations and digests
// over that canonical form.

const devinDumpLimit = 512 << 20

// SQLite opens its own database and WAL paths, outside os.Root. Validate every
// selected path component before constructing the read-only URI. This rejects
// symlinked sources and treats URI metacharacters as literal filename bytes.
func devinDSN(root, path string) (string, error) {
	if !filepath.IsLocal(path) || !strings.HasSuffix(path, ".db") {
		return "", errors.New("invalid_source_path")
	}
	selected, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("source_unavailable")
	}
	defer selected.Close()
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := range parts {
		part := filepath.Join(parts[:i+1]...)
		info, err := selected.Lstat(part)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return "", errors.New("invalid_source_path")
		}
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		if info, err := selected.Lstat(sidecar); err == nil {
			if !info.Mode().IsRegular() {
				return "", errors.New("invalid_source_path")
			}
		} else if !os.IsNotExist(err) {
			return "", errors.New("invalid_source_path")
		}
	}
	abs, err := filepath.Abs(filepath.Join(root, path))
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}).String(), nil
}

// DevinDump renders a Devin desktop acp-messages database as canonical JSONL.
// A ro SQLite connection observes committed state only, so the dump is a
// consistent snapshot. The desktop rewrites rows in place while a session is
// active, so callers should gate on DevinQuiet first; a rewrite of already
// captured rows between quiet snapshots surfaces as a continuity gap.
// DevinWal reports the WAL's mtime. The store is "hot" when the WAL was
// touched within the last thirty seconds — capture defers while hot.
func DevinWal(root, path string) (time.Time, bool) {
	if _, err := devinDSN(root, path); err != nil {
		return time.Time{}, false
	}
	st, err := os.Stat(filepath.Join(root, path) + "-wal")
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), time.Since(st.ModTime()) < 30*time.Second
}

// DevinActivity probes a hot store for conversation growth. The desktop app
// writes bookkeeping pages to every open session's WAL, so file churn alone
// is not session activity; only a changing message count or newest position
// is. A read-only probe on a hot WAL may race a commit — it is a liveness
// hint, never stored state.
func DevinActivity(ctx context.Context, root, path string) (count, maxPos int, err error) {
	dsn, err := devinDSN(root, path)
	if err != nil {
		return 0, 0, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	err = db.QueryRowContext(ctx, "SELECT count(*), coalesce(max(position),0) FROM messages").Scan(&count, &maxPos)
	return count, maxPos, err
}

func DevinDump(ctx context.Context, root, path string) ([]byte, error) {
	dsn, err := devinDSN(root, path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var out bytes.Buffer
	// Only stable meta fields enter the canonical stream: counters and live
	// session config churn on every write and would invalidate the prefix.
	var infoTitle, schemaVersion string
	_ = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&schemaVersion)
	var infoRaw string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='info'").Scan(&infoRaw); err == nil {
		var info object
		if json.Unmarshal([]byte(infoRaw), &info) == nil {
			infoTitle = str(info, "title")
		}
	}
	meta := map[string]any{"schema_version": schemaVersion, "info": map[string]any{"title": infoTitle}}
	encoded, _ := json.Marshal(meta)
	fmt.Fprintf(&out, `{"kind":"meta","payload":%s}`+"\n", encoded)
	rows, err := tx.QueryContext(ctx, "SELECT position,kind,payload FROM messages ORDER BY position")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pos int64
		var kind, payload string
		if err := rows.Scan(&pos, &kind, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		kindJSON, _ := json.Marshal(kind)
		position := strconv.FormatInt(pos, 10)
		lineBytes := len(`{"kind":`) + len(kindJSON) + len(`,"position":`) + len(position) + len(`,"payload":`) + len(payload) + len("}\n")
		if lineBytes > devinDumpLimit-out.Len() {
			rows.Close()
			return nil, errors.New("source_too_large")
		}
		out.WriteString(`{"kind":`)
		out.Write(kindJSON)
		out.WriteString(`,"position":`)
		out.WriteString(position)
		out.WriteString(`,"payload":`)
		out.WriteString(payload)
		out.WriteString("}\n")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return out.Bytes(), nil
}

// identifyDevin proves store shape without emitting conversation text: a meta
// row count and the message count are metadata, never bodies.
func identifyDevin(ctx context.Context, root, path string) (Identity, error) {
	out := Identity{Evidence: "sqlite_store_shape", ProviderVersion: "unknown"}
	dsn, err := devinDSN(root, path)
	if err != nil {
		return out, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return out, errors.New("source_unavailable")
	}
	defer db.Close()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('meta','messages')").Scan(&tables); err != nil {
		return out, errors.New("source_unavailable")
	}
	if tables != 2 {
		return out, errors.New("conversation_identity_unavailable")
	}
	var messages int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&messages); err != nil {
		return out, errors.New("source_unavailable")
	}
	if messages == 0 {
		return out, errors.New("empty_session_store")
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err == nil && version != "" {
		out.ProviderVersion = "acp-store-" + version
	}
	out.ConversationID = strings.TrimSuffix(filepath.Base(path), ".db")
	if out.ConversationID == "" || len(out.ConversationID) > 256 {
		return out, errors.New("conversation_identity_unavailable")
	}
	return out, nil
}

// normalizeDevin maps canonical store lines onto the record contract. The meta
// line carries the session's native title; agent output becomes messages and
// notes; tool calls keep their raw payload. This store has no timestamps.
func normalizeDevin(n object, r *sessionrecord.Record) {
	payload := obj(n, "payload")
	switch r.NativeKind {
	case "meta":
		if info := obj(payload, "info"); info != nil {
			if t := str(info, "title"); t != "" {
				r.Kind = "title"
				r.Body.Text = t
			}
		}
		if r.Kind == "opaque_record" {
			// A title-less store still carries session metadata; a note keeps
			// the transcript readable instead of an unexplained opaque row.
			r.Kind, r.Body.Text = "native_note", "session store metadata"
		}
	case "agent_message":
		if text := acpText(payload); text != "" {
			r.Kind = "message"
			r.Role = textPtr("assistant")
			r.Body.Text = text
		}
	case "user_message":
		if text := acpText(payload); text != "" {
			r.Kind = "message"
			r.Role = textPtr("user")
			r.Body.Text = text
		}
	case "agent_thought", "plan":
		r.Kind = "native_note"
		if r.NativeKind == "agent_thought" {
			r.Body.Text = acpText(payload)
		} else {
			r.Body.Text = planText(payload)
		}
	case "tool_call":
		content := obj(payload, "content")
		part := sessionrecord.Part{Type: "tool_call", ID: str(content, "toolCallId"), Name: str(content, "title"), Data: n["payload"]}
		if part.Name == "" {
			part.Name = str(content, "kind")
		}
		if input := obj(content, "rawInput"); input != nil {
			part.Text = str(input, "command")
			exts := map[string]json.RawMessage{}
			if wd := str(input, "workdir"); wd != "" && len(wd) <= 4096 {
				v, _ := json.Marshal(wd)
				exts["native_cwd"] = v
			}
			if fp := str(input, "file_path"); strings.HasPrefix(fp, "/") && len(fp) <= 4096 {
				v, _ := json.Marshal(fp)
				exts["native_file_path"] = v
			} else if dp := str(input, "path"); strings.HasPrefix(dp, "/") && len(dp) <= 4096 {
				// `path` names a directory for list/search tools.
				v, _ := json.Marshal(dp)
				exts["native_dir"] = v
			}
			if len(exts) > 0 {
				r.Extensions = exts
			}
		}
		r.Kind = "tool_call"
		r.Body.Parts = []sessionrecord.Part{part}
	}
}

// acpText joins ACP session-update chunks into one display string.
func acpText(payload object) string {
	var chunks []object
	if json.Unmarshal(payload["content"], &chunks) != nil {
		return ""
	}
	var texts []string
	for _, chunk := range chunks {
		if c := obj(chunk, "content"); c != nil {
			if s := str(c, "text"); s != "" {
				texts = append(texts, s)
			}
		}
	}
	return strings.Join(texts, "")
}

func planText(payload object) string {
	content := obj(payload, "content")
	var entries []object
	if json.Unmarshal(content["entries"], &entries) != nil {
		return ""
	}
	var texts []string
	for _, e := range entries {
		if s := str(e, "content"); s != "" {
			texts = append(texts, s)
		}
	}
	return strings.Join(texts, " · ")
}
