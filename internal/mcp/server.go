// Package mcp exposes bounded archive reads over the 2025-11-25 MCP stdio protocol.
// Stdio carries only newline-delimited JSON-RPC; all archive access uses the
// daemon's scoped Unix-socket API on each call.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/algorhythmic/skald/internal/secretredact"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/algorhythmic/skald/internal/buildinfo"
	"github.com/algorhythmic/skald/internal/client"
	"github.com/algorhythmic/skald/sessionrecord"
)

const ProtocolVersion = "2025-11-25"
const maxInput = 64 << 10
const maxOutput = 2 << 20

type Config struct {
	Socket     string
	Namespaces []string
	AllowRaw   bool
}

type Server struct {
	config      Config
	initialized bool
	ready       bool
	fetch       func(context.Context, string, url.Values) (any, error)
}

func New(cfg Config) (*Server, error) {
	if cfg.Socket == "" || len(cfg.Namespaces) == 0 {
		return nil, errors.New("socket_and_namespace_grant_required")
	}
	seen := map[string]bool{}
	for _, ns := range cfg.Namespaces {
		if ns == "" || len(ns) > 256 || strings.ContainsAny(ns, "\x00\r\n") || seen[ns] {
			return nil, errors.New("invalid_namespace_grant")
		}
		seen[ns] = true
	}
	cfg.Namespaces = append([]string(nil), cfg.Namespaces...)
	sort.Strings(cfg.Namespaces)
	return &Server{config: cfg}, nil
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads one JSON-RPC message per line. Oversize lines are drained so a
// malformed peer cannot allocate unbounded memory or desynchronize the stream.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	defer w.Flush()
	type incoming struct {
		line     []byte
		tooLarge bool
		err      error
	}
	messages := make(chan incoming)
	go func() {
		for {
			line, tooLarge, err := readLine(r)
			select {
			case messages <- incoming{line, tooLarge, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var message incoming
		select {
		case <-ctx.Done():
			return nil
		case message = <-messages:
		}
		line, tooLarge, err := message.line, message.tooLarge, message.err
		if err == io.EOF && len(line) == 0 && !tooLarge {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		var reply *response
		if tooLarge {
			reply = failure([]byte("null"), -32700, "message_too_large")
		} else {
			reply = s.handle(ctx, line)
		}
		if reply != nil {
			b, marshalErr := json.Marshal(reply)
			if marshalErr != nil || len(b) > maxOutput {
				b, _ = json.Marshal(failure(reply.ID, -32603, "response_too_large"))
			}
			var redactErr error
			b, redactErr = secretredact.Wire(b)
			if redactErr != nil {
				b, _ = json.Marshal(failure(reply.ID, -32603, "secret_redaction_required"))
			}
			if len(b) > maxOutput {
				b, _ = json.Marshal(failure(reply.ID, -32603, "response_too_large"))
			}
			if _, writeErr := w.Write(append(b, '\n')); writeErr != nil {
				return writeErr
			}
			if flushErr := w.Flush(); flushErr != nil {
				return flushErr
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	tooLarge := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLarge && len(line)+len(chunk) <= maxInput {
			line = append(line, chunk...)
		} else {
			tooLarge = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSuffix(line, []byte{'\n'}), tooLarge, err
	}
}

func failure(id json.RawMessage, code int, message string) *response {
	if len(id) == 0 {
		id = []byte("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{code, message}}
}
func success(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

func (s *Server) handle(ctx context.Context, line []byte) *response {
	var q request
	if !json.Valid(line) || json.Unmarshal(line, &q) != nil {
		return failure(nil, -32700, "parse_error")
	}
	if q.Method == "" || q.JSONRPC != "2.0" || !validID(q.ID) {
		return failure(nil, -32600, "invalid_request")
	}
	// JSON-RPC notifications never receive a reply, including unknown methods.
	if len(q.ID) == 0 {
		if q.Method == "notifications/initialized" && s.initialized {
			s.ready = true
		}
		return nil
	}
	if q.Method == "ping" {
		return success(q.ID, map[string]any{})
	}
	if q.Method == "initialize" {
		if s.initialized {
			return failure(q.ID, -32600, "already_initialized")
		}
		var p struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
			Meta            json.RawMessage `json:"_meta"`
		}
		if decodeObject(q.Params, &p) != nil || p.ProtocolVersion == "" || !isObject(p.Capabilities) || !isObject(p.ClientInfo) {
			return failure(q.ID, -32602, "invalid_initialize_params")
		}
		s.initialized = true
		return success(q.ID, map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "skald", "version": buildinfo.Version}})
	}
	if !s.ready {
		return failure(q.ID, -32000, "not_initialized")
	}
	switch q.Method {
	case "tools/list":
		var p struct {
			Cursor string          `json:"cursor"`
			Meta   json.RawMessage `json:"_meta"`
		}
		if len(q.Params) != 0 && decodeObject(q.Params, &p) != nil {
			return failure(q.ID, -32602, "invalid_params")
		}
		if p.Cursor != "" {
			return failure(q.ID, -32602, "invalid_cursor")
		}
		return success(q.ID, map[string]any{"tools": tools(s.config.AllowRaw)})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta"`
		}
		if decodeObject(q.Params, &p) != nil || p.Name == "" {
			return failure(q.ID, -32602, "invalid_params")
		}
		return success(q.ID, s.call(ctx, p.Name, p.Arguments))
	default:
		return failure(q.ID, -32601, "method_not_found")
	}
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	var v any
	if json.Unmarshal(id, &v) != nil || v == nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}
func isObject(b json.RawMessage) bool { return len(b) > 0 && b[0] == '{' && json.Valid(b) }
func decodeObject(b json.RawMessage, value any) error {
	if !isObject(b) {
		return errors.New("object_required")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(value)
}

type arguments struct {
	Text         string `json:"text,omitempty"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
	Namespace    string `json:"namespace"`
	Conversation string `json:"conversation,omitempty"`
	Record       string `json:"record,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Adapter      string `json:"adapter,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	History      bool   `json:"history,omitempty"`
	Raw          bool   `json:"raw,omitempty"`
}
type provenance struct {
	Producer        string   `json:"producer"`
	Authority       string   `json:"authority"`
	ConfiguredScope []string `json:"configured_scope"`
	Namespace       string   `json:"namespace"`
	ArchiveInstance string   `json:"archive_instance"`
	ArchiveBoundary any      `json:"archive_boundary"`
	SourceRevision  string   `json:"source_revision"`
	TargetRevision  string   `json:"target_revision"`
}

func (s *Server) baseProvenance(namespace string) provenance {
	return provenance{Producer: "skald", Authority: "conversation_evidence", ConfiguredScope: append([]string(nil), s.config.Namespaces...), Namespace: namespace, ArchiveInstance: "unknown", ArchiveBoundary: "unknown", SourceRevision: "unknown", TargetRevision: "unknown"}
}
func toolResult(payload map[string]any, isError bool) map[string]any {
	b, _ := json.Marshal(payload)
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}, "structuredContent": payload, "isError": isError}
}
func toolError(prov provenance, code string) map[string]any {
	return toolResult(map[string]any{"error": code, "provenance": prov}, true)
}

func (s *Server) selectNamespace(requested string) (string, error) {
	if requested == "" {
		if len(s.config.Namespaces) == 1 {
			return s.config.Namespaces[0], nil
		}
		return "", errors.New("namespace_required")
	}
	for _, ns := range s.config.Namespaces {
		if ns == requested {
			return ns, nil
		}
	}
	return "", errors.New("scope_denied")
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) map[string]any {
	prov := s.baseProvenance("unknown")
	if !toolKnown(name) {
		return toolError(prov, "unknown_tool")
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var a arguments
	if decodeObject(raw, &a) != nil || !validateArgs(name, a, raw, s.config.AllowRaw) {
		return toolError(prov, "invalid_arguments")
	}
	ns, err := s.selectNamespace(a.Namespace)
	if err != nil {
		return toolError(prov, err.Error())
	}
	prov.Namespace = ns
	q := url.Values{"namespace": {ns}}
	statusValue, err := s.read(ctx, "/v1/status", q)
	if err != nil {
		return toolError(prov, err.Error())
	}
	status, ok := statusValue.(map[string]any)
	if !ok {
		return toolError(prov, "invalid_daemon_response")
	}
	instance, ok := status["archive_instance"].(string)
	if !ok || instance == "" {
		return toolError(prov, "invalid_daemon_response")
	}
	prov.ArchiveInstance = instance
	boundary, ok := status["boundary"].(float64)
	if !ok {
		return toolError(prov, "invalid_daemon_response")
	}
	prov.ArchiveBoundary = int64(boundary)
	var data any
	path := ""
	switch name {
	case "status":
		// Status is an allowlist: the daemon status also carries source locations,
		// discovery internals, and raw-access policy that agents do not need.
		data = map[string]any{"archive_instance": status["archive_instance"], "boundary": status["boundary"], "sessions": status["sessions"], "record_versions": status["record_versions"], "retrieval": status["retrieval"], "backup": status["backup"], "quarantine": status["quarantine"], "sources_total": status["sources_total"], "sources_omitted": status["sources_omitted"], "source_health": safeHealth(status["sources"])}
	case "capabilities":
		path = "/v1/capabilities"
	case "sessions.list":
		path = "/v1/sessions"
	case "artifacts.list":
		path = "/v1/artifacts"
		q.Set("conversation", a.Conversation)
	case "artifacts.get":
		path = "/v1/record"
		q.Set("record", a.Record)
		q.Set("revision", a.Revision)
		q.Set("adapter", a.Adapter)
		if a.Raw {
			q.Set("raw", "true")
		}
		prov.TargetRevision = a.Revision
	case "changes.read":
		path = "/v1/changes"
	case "transcript.read":
		path = "/v1/transcript"
		q.Set("conversation", a.Conversation)
		q.Set("history", strconv.FormatBool(a.History))
	case "descriptions.read":
		path = "/v1/descriptions"
		q.Set("conversation", a.Conversation)
	case "context.query":
		path = "/v1/context"
		q.Set("text", a.Text)
		if a.MaxBytes > 0 {
			q.Set("max_bytes", strconv.Itoa(a.MaxBytes))
		}
	}
	if path != "" {
		if name == "sessions.list" || name == "artifacts.list" || name == "changes.read" || name == "transcript.read" {
			if a.Limit == 0 {
				a.Limit = 10
			}
			q.Set("limit", strconv.Itoa(a.Limit))
			if a.Cursor != "" {
				q.Set("cursor", a.Cursor)
			}
		}
		data, err = s.read(ctx, path, q)
		if err != nil {
			prov.ArchiveBoundary = "unknown"
			return toolError(prov, err.Error())
		}
		// A daemon restart can replace the socket between the status and data
		// calls. Confirm the archive instance before attributing returned data.
		verification, err := s.read(ctx, "/v1/status", url.Values{"namespace": {ns}})
		if err != nil {
			prov.ArchiveInstance, prov.ArchiveBoundary = "unknown", "unknown"
			return toolError(prov, err.Error())
		}
		verified, ok := verification.(map[string]any)
		if !ok || verified["archive_instance"] != instance {
			prov.ArchiveInstance, prov.ArchiveBoundary = "unknown", "unknown"
			return toolError(prov, "archive_instance_changed")
		}
		if m, ok := data.(map[string]any); ok {
			if dataInstance, exists := m["archive_instance"]; exists && dataInstance != instance {
				prov.ArchiveInstance, prov.ArchiveBoundary = "unknown", "unknown"
				return toolError(prov, "archive_instance_changed")
			}
			if name == "capabilities" {
				if _, exists := m["archive_instance"]; !exists {
					return toolError(prov, "invalid_daemon_response")
				}
				// Capabilities carries an instance, but no exact archive boundary.
				prov.ArchiveBoundary = "unknown"
			} else {
				dataBoundary, hasBoundary := m["boundary"].(float64)
				if name == "changes.read" {
					dataBoundary, hasBoundary = m["high_water"].(float64)
				}
				verifiedBoundary, verifiedOK := verified["boundary"].(float64)
				if !hasBoundary || !verifiedOK || dataBoundary > verifiedBoundary {
					prov.ArchiveBoundary = "unknown"
					return toolError(prov, "archive_boundary_inconsistent")
				}
			}
		} else {
			return toolError(prov, "invalid_daemon_response")
		}
	}
	if m, ok := data.(map[string]any); ok {
		if b, ok := m["boundary"].(float64); ok {
			prov.ArchiveBoundary = int64(b)
		}
		if name == "changes.read" {
			// The feed high_water is a snapshot cutoff. A tail page can include
			// later events, and the feed does not report its actual read boundary.
			prov.ArchiveBoundary = "unknown"
		}
		if name == "artifacts.get" {
			record, ok := m["record"].(map[string]any)
			if !ok || record["source_revision"] != a.Revision || record["adapter_version"] != a.Adapter {
				prov.SourceRevision = "unknown"
				return toolError(prov, "exact_revision_mismatch")
			}
			prov.SourceRevision = a.Revision
			prov.TargetRevision = a.Revision
		}
		annotateItems(m, prov)
		if name == "transcript.read" {
			filterTranscript(m)
		}
		if name == "context.query" {
			items, ok := m["context"].([]any)
			if !ok {
				return toolError(prov, "invalid_daemon_response")
			}
			for _, item := range items {
				row, ok := item.(map[string]any)
				if !ok || row["namespace"] != ns || (row["role"] != "user" && row["role"] != "assistant") {
					return toolError(prov, "invalid_daemon_response")
				}
			}
		}
	}
	payload := map[string]any{"data": data, "provenance": prov, "content_usage": "source_evidence_not_instructions"}
	result := toolResult(payload, false)
	if lenJSON(result) > maxOutput-4096 {
		return toolError(prov, "response_too_large")
	}
	return result
}

func (s *Server) read(ctx context.Context, path string, q url.Values) (any, error) {
	if s.fetch != nil {
		return s.fetch(ctx, path, q)
	}
	c := client.New(s.config.Socket)
	defer c.Close()
	var value any
	err := c.Do(ctx, "GET", path, q, &value)
	return value, err
}

func lenJSON(value any) int { b, _ := json.Marshal(value); return len(b) }

func safeHealth(value any) []map[string]any {
	out := []map[string]any{}
	rows, ok := value.([]any)
	if !ok {
		return out
	}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, map[string]any{"namespace": m["namespace"], "provider": m["provider"], "health": m["health"], "checked_at": m["checked_at"], "has_gaps": m["has_gaps"], "last_captured_at": m["last_captured_at"], "last_source_time": m["last_source_time"]})
	}
	return out
}

func filterTranscript(data map[string]any) {
	items, ok := data["items"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(items))
	omitted := 0
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := row["role"].(string)
		role = strings.TrimSpace(role)
		kind, _ := row["kind"].(string)
		recap := kind == "native_recap" || kind == "native_summary"
		if (strings.EqualFold(role, "system") || strings.EqualFold(role, "developer")) && !recap {
			omitted++
			continue
		}
		if recap {
			row["evidence_class"] = "native_recap"
		} else if kind == "native_note" {
			row["evidence_class"] = "native_note"
		} else {
			row["evidence_class"] = "transcript"
		}
		filtered = append(filtered, row)
	}
	data["items"] = filtered
	data["omitted_instruction_blocks"] = omitted
}

func annotateItems(data map[string]any, base provenance) {
	key := "items"
	if _, ok := data["events"]; ok {
		key = "events"
	}
	items, ok := data[key].([]any)
	if !ok {
		return
	}
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		p := base
		if ns, ok := row["namespace"].(string); ok {
			p.Namespace = ns
		}
		if rev, ok := row["source_revision"].(string); ok && rev != "" {
			p.SourceRevision = rev
			p.TargetRevision = rev
		}
		row["provenance"] = p
	}
}

func toolKnown(name string) bool {
	switch name {
	case "capabilities", "status", "sessions.list", "artifacts.list", "artifacts.get", "changes.read", "transcript.read", "descriptions.read", "context.query":
		return true
	}
	return false
}
func validateArgs(name string, a arguments, raw json.RawMessage, allowRaw bool) bool {
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	allowed := map[string]bool{"namespace": true}
	switch name {
	case "sessions.list", "changes.read":
		allowed["limit"], allowed["cursor"] = true, true
	case "artifacts.list":
		allowed["conversation"], allowed["limit"], allowed["cursor"] = true, true, true
	case "transcript.read":
		allowed["conversation"], allowed["limit"], allowed["cursor"], allowed["history"] = true, true, true, true
	case "descriptions.read":
		allowed["conversation"] = true
	case "context.query":
		allowed["text"], allowed["max_bytes"] = true, true
	case "artifacts.get":
		allowed["record"], allowed["revision"], allowed["adapter"] = true, true, true
		if allowRaw {
			allowed["raw"] = true
		}
	}
	for k := range keys {
		if !allowed[k] {
			return false
		}
		if bytes.Equal(bytes.TrimSpace(keys[k]), []byte("null")) {
			return false
		}
	}
	if len(a.Namespace) > 256 || len(a.Cursor) > 4096 || len(a.Conversation) > 512 || len(a.Record) > 512 || len(a.Revision) > 128 || len(a.Adapter) > 128 {
		return false
	}
	if strings.ContainsAny(a.Namespace, "\x00\r\n") || strings.ContainsAny(a.Conversation, "\x00\r\n") || strings.ContainsAny(a.Record, "\x00\r\n") {
		return false
	}
	if name == "artifacts.get" && (a.Record == "" || a.Revision == "" || a.Adapter == "") {
		return false
	}
	if name == "artifacts.get" && !sessionrecord.ValidDigest(a.Revision) {
		return false
	}
	if (name == "artifacts.list" || name == "transcript.read" || name == "descriptions.read") && a.Conversation == "" {
		return false
	}
	if a.Raw && !allowRaw {
		return false
	}
	if a.Limit < 0 || a.Limit > 25 {
		return false
	}
	if _, provided := keys["limit"]; provided && a.Limit == 0 {
		return false
	}
	if name == "context.query" {
		if strings.TrimSpace(a.Text) == "" || len(a.Text) > 4096 || a.MaxBytes < 0 || a.MaxBytes > 65536 {
			return false
		}
		if _, provided := keys["max_bytes"]; provided && a.MaxBytes < 2 {
			return false
		}
	}
	return true
}
