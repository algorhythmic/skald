// Package retrieval is an explicitly enabled, pinned local Braid adapter.
package retrieval

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessionrecord"
)

type Config struct {
	Executable       string `json:"executable"`
	SHA256           string `json:"sha256"`
	Directory        string `json:"directory"`
	TimeoutMS        int    `json:"timeout_ms"`
	MaxScopes        int    `json:"max_scopes"`
	MaxCacheBytes    int64  `json:"max_cache_bytes"`
	MaxNodes         int    `json:"max_nodes"`
	MaxSnapshotBytes int    `json:"max_snapshot_bytes"`
}
type Request struct {
	Text     string `json:"text"`
	MaxBytes int    `json:"max_bytes"`
}
type Item struct {
	ID        string            `json:"id"`
	Text      string            `json:"text"`
	Ref       sessionrecord.Ref `json:"ref"`
	Adapter   string            `json:"adapter_version"`
	Namespace string            `json:"namespace"`
	Role      string            `json:"role"`
}
type Dataset struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}
type Response struct {
	Status         string  `json:"status"`
	Context        []Item  `json:"context"`
	ContextBytes   int     `json:"context_bytes"`
	Boundary       int64   `json:"boundary"`
	Dataset        Dataset `json:"dataset"`
	RankingVersion string  `json:"ranking_version,omitempty"`
	Coverage       string  `json:"coverage"`
}
type Engine struct {
	mu         sync.Mutex
	config     Config
	store      *archive.Store
	entries    map[string]*publication
	lease      *os.File
	cacheRoot  *os.Root
	cleanupErr error
}
type publication struct {
	client             *client
	directory, content string
	dataset            Dataset
	ranking            string
	used               time.Time
	generation         int64
	snapshot           archive.RetrievalSnapshot
	allowed            []string
	lookup             map[string]archive.RetrievalRecord
}

func New(config Config, store *archive.Store) (*Engine, error) {
	if !filepath.IsAbs(config.Executable) || !filepath.IsAbs(config.Directory) || len(config.SHA256) != 64 || store == nil {
		return nil, errors.New("invalid_retrieval_configuration")
	}
	if _, err := hex.DecodeString(config.SHA256); err != nil {
		return nil, errors.New("invalid_retrieval_binary_digest")
	}
	if config.TimeoutMS == 0 {
		config.TimeoutMS = 5000
	}
	if config.TimeoutMS < 1 || config.TimeoutMS > 60000 {
		return nil, errors.New("invalid_retrieval_timeout")
	}
	if config.MaxScopes == 0 {
		config.MaxScopes = 1
	}
	if config.MaxScopes < 1 || config.MaxScopes > 16 {
		return nil, errors.New("invalid_retrieval_cache_limit")
	}
	if config.MaxCacheBytes == 0 {
		config.MaxCacheBytes = 256 << 20
	}
	if config.MaxCacheBytes < 1<<20 || config.MaxCacheBytes > 1<<30 {
		return nil, errors.New("invalid_retrieval_cache_limit")
	}
	if config.MaxNodes == 0 {
		config.MaxNodes = 100000
	}
	if config.MaxSnapshotBytes == 0 {
		config.MaxSnapshotBytes = 128 << 20
	}
	if config.MaxNodes < 1 || config.MaxNodes > 250000 || config.MaxSnapshotBytes < 1 || config.MaxSnapshotBytes > 256<<20 {
		return nil, errors.New("invalid_retrieval_snapshot_limit")
	}
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		return nil, err
	}
	lease, root, err := leaseCache(config.Directory)
	if err != nil {
		return nil, err
	}
	e := &Engine{config: config, store: store, entries: map[string]*publication{}, lease: lease, cacheRoot: root}
	if err := e.purgeOrphans(); err != nil {
		root.Close()
		lease.Close()
		return nil, err
	}
	return e, nil
}
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lease == nil {
		return nil
	}
	err := e.invalidate()
	err = errors.Join(err, e.cacheRoot.Close(), e.lease.Close())
	e.entries = nil
	e.lease = nil
	e.cacheRoot = nil
	return err
}
func (e *Engine) Query(ctx context.Context, namespaces []string, generation int64, request Request) (Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := Response{Status: "unavailable", Context: []Item{}, ContextBytes: 2, Coverage: "unavailable"}
	if err := e.cacheIdentity(); err != nil {
		return out, err
	}
	if e.cleanupErr != nil {
		return out, e.cleanupErr
	}
	if strings.TrimSpace(request.Text) == "" || len(request.Text) > 16384 {
		return out, errors.New("invalid_retrieval_query")
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = 16384
	}
	if request.MaxBytes < 2 || request.MaxBytes > 1<<20 {
		return out, errors.New("invalid_retrieval_max_bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.config.TimeoutMS)*time.Millisecond)
	defer cancel()
	// Scope generations never share datasets; remove revoked cached publications.
	for k, p := range e.entries {
		if p.generation != generation {
			if err := e.remove(k); err != nil {
				return out, err
			}
		}
	}
	identity, err := e.store.RetrievalIdentity(ctx, namespaces, generation)
	if err != nil {
		return out, err
	}
	ns := append([]string{}, namespaces...)
	slices.Sort(ns)
	scope := sessionrecord.Key("braid-scope", append([]string{identity.Instance, strconv.FormatInt(generation, 10)}, ns...)...)
	var snapshot archive.RetrievalSnapshot
	if cached := e.entries[scope]; cached != nil && cached.snapshot.CorpusRevision == identity.CorpusRevision {
		snapshot = cached.snapshot
	} else {
		snapshot, err = e.store.RetrievalSnapshot(ctx, namespaces, generation, e.config.MaxNodes, e.config.MaxSnapshotBytes)
		if err != nil {
			if err.Error() == "retrieval_snapshot_too_large" {
				out.Coverage = "corpus_exceeds_configured_limit"
			}
			return out, err
		}
	}
	out.Boundary = snapshot.Boundary
	content := ""
	if cached := e.entries[scope]; cached != nil && cached.snapshot.CorpusRevision == snapshot.CorpusRevision {
		content = cached.content
	} else {
		corpus, _ := json.Marshal(snapshot.Records)
		sum := sha256.Sum256(corpus)
		content = hex.EncodeToString(sum[:])
	}
	p := e.entries[scope]
	if p != nil && p.content != content {
		if cleanupErr := e.remove(scope); cleanupErr != nil {
			return out, errors.Join(err, cleanupErr)
		}
		p = nil
	}
	if p == nil {
		if len(e.entries) >= e.config.MaxScopes {
			var oldest string
			for k, v := range e.entries {
				if oldest == "" || v.used.Before(e.entries[oldest].used) {
					oldest = k
				}
			}
			if err := e.remove(oldest); err != nil {
				return out, err
			}
		}
		p, err = e.publish(ctx, scope, content, generation, snapshot.Records)
		if err != nil {
			return out, err
		}
		e.entries[scope] = p
	}
	if p.snapshot.Instance != "" {
		snapshot.Records = p.snapshot.Records
	}
	p.snapshot = snapshot
	p.used = time.Now()
	out.Dataset = p.dataset
	out.RankingVersion = p.ranking
	if p.lookup == nil {
		p.allowed = make([]string, 0, len(snapshot.Records))
		p.lookup = map[string]archive.RetrievalRecord{}
		for _, r := range snapshot.Records {
			p.allowed = append(p.allowed, r.ID)
			p.lookup[r.ID] = r
		}
	}
	allowed, lookup := p.allowed, p.lookup
	var result struct {
		Dataset Dataset `json:"dataset"`
		Context []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"context"`
		ContextBytes int `json:"context_bytes"`
	}
	query := map[string]any{"dataset_id": scope, "expected_revision": p.dataset.Revision, "text": request.Text, "filters": map[string]any{"allowed_ids": allowed}, "requires": map[string][]string{}, "mandatory_ids": []string{}, "weights": map[string]int{"lexical": 1, "dense": 0, "graph": 0, "temporal": 0}, "budget": map[string]any{"max": request.MaxBytes, "unit": "adapter_bytes", "estimator": "conservative-context-v1"}}
	err = p.client.call(ctx, "export_context", map[string]any{"timeout_ms": max(1, e.config.TimeoutMS), "export": map[string]any{"query": query, "output": map[string]any{"fields": []string{"id", "text"}, "attrs": []string{}, "max_bytes": request.MaxBytes}}}, &result)
	if err != nil {
		if cleanupErr := e.remove(scope); cleanupErr != nil {
			return out, errors.Join(err, cleanupErr)
		}
		return out, err
	}
	if result.Dataset != p.dataset {
		return out, errors.New("retrieval_dataset_changed")
	}
	delivered := []archive.RetrievalRecord{}
	items := []Item{}
	seen := map[string]bool{}
	for _, item := range result.Context {
		r, ok := lookup[item.ID]
		if !ok || seen[item.ID] || r.Text != item.Text {
			return out, errors.New("invalid_retrieval_citation")
		}
		seen[item.ID] = true
		delivered = append(delivered, r)
		items = append(items, Item{ID: r.ID, Text: r.Text, Ref: r.Ref, Adapter: r.Adapter, Namespace: r.Namespace, Role: r.Role})
	}
	items, contextBytes := fitContext(items, request.MaxBytes)
	delivered = delivered[:len(items)]
	if err := e.store.ValidateRetrieval(ctx, namespaces, snapshot, delivered); err != nil {
		if cleanupErr := e.remove(scope); cleanupErr != nil {
			return out, errors.Join(err, cleanupErr)
		}
		out.Context = []Item{}
		out.Status = "stale"
		return out, err
	}
	out.Context = items
	out.ContextBytes = contextBytes
	out.Status = "ok"
	if len(out.Context) == 0 {
		out.Status = "no_answer"
	}
	out.Coverage = "complete"
	return out, nil
}

func (e *Engine) publish(ctx context.Context, scope, content string, generation int64, records []archive.RetrievalRecord) (published *publication, retErr error) {
	dir, err := os.MkdirTemp(e.config.Directory, "publication-")
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			if err := e.purgePublication(filepath.Base(dir)); err != nil {
				e.cleanupErr = err
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	existingBytes, err := e.cacheBytes()
	if err != nil {
		return nil, err
	}
	remaining := e.config.MaxCacheBytes - existingBytes
	if remaining < 1<<20 {
		return nil, errors.New("retrieval_cache_too_large")
	}
	// Reserve half the remaining cache for the WAL and publication overhead.
	// SQLite enforces the database-page cap during each apply transaction.
	storeURI := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "index.sqlite"), RawQuery: "_pragma=max_page_count(" + strconv.FormatInt(remaining/2/4096, 10) + ")"}).String()
	config := map[string]any{"version": 1, "dataset_id": scope, "store": map[string]string{"backend": "sqlite", "path": storeURI}, "embed": map[string]string{"provider": "none"}, "defaults": map[string]any{"weights": map[string]int{"lexical": 1, "dense": 0, "graph": 0, "temporal": 0}, "mmr_lambda": 1}, "candidate_limit": 100}
	wire, _ := json.Marshal(config)
	path := filepath.Join(dir, "braid.yaml")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		return nil, err
	}
	c, err := start(ctx, e.config, path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			c.close()
		}
	}()
	capabilities := []string{"query", "strict_requests", "structured_errors", "dataset_binding", "dataset", "replace_snapshot", "apply", "allowed_ids", "get_many", "read_revision", "required_context", "context_export", "query_timeout"}
	var hello struct {
		Protocol     int      `json:"protocol_version"`
		Capabilities []string `json:"capabilities"`
		Max          int      `json:"max_request_bytes"`
		Ranking      string   `json:"ranking_version"`
		Dataset      Dataset  `json:"dataset"`
	}
	if err := c.call(ctx, "hello", map[string]any{"versions": []int{1}, "required_capabilities": capabilities}, &hello); err != nil {
		return nil, err
	}
	if hello.Protocol != 1 || hello.Dataset.ID != scope || hello.Dataset.Revision != 0 || hello.Ranking != "mmr-admitted-dependencies-v2" {
		return nil, errors.New("retrieval_negotiation_failed")
	}
	for _, capability := range capabilities {
		if !slices.Contains(hello.Capabilities, capability) {
			return nil, errors.New("retrieval_capability_missing")
		}
	}
	var dataset Dataset
	// Build the isolated staging dataset in bounded revision-checked frames.
	// No query or cache entry can observe a partially published generation.
	if err := c.call(ctx, "replace_snapshot", map[string]any{"snapshot": map[string]any{"dataset_id": scope, "expected_revision": 0, "batch": map[string]any{"nodes": []any{}, "edges": []any{}, "embeddings": []any{}}}}, &dataset); err != nil {
		return nil, err
	}
	frameLimit := min(12<<20, hello.Max-1024)
	if frameLimit < 1024 {
		return nil, errors.New("retrieval_frame_limit_too_small")
	}
	for start := 0; start < len(records); {
		nodes := []json.RawMessage{}
		size := 0
		end := start
		for end < len(records) {
			r := records[end]
			node, _ := json.Marshal(map[string]any{"id": r.ID, "type": "message", "text": r.Text, "ts": r.Timestamp, "cost": len(r.Text) + 512, "attrs": map[string]string{"role": r.Role}})
			if len(node) > frameLimit {
				return nil, errors.New("retrieval_record_exceeds_frame_limit")
			}
			if size+len(node)+1 > frameLimit {
				break
			}
			nodes = append(nodes, node)
			size += len(node) + 1
			end++
		}
		if end == start {
			return nil, errors.New("retrieval_publication_too_large")
		}
		update := map[string]any{"dataset_id": scope, "expected_revision": dataset.Revision, "upsert": map[string]any{"nodes": nodes, "edges": []any{}, "embeddings": []any{}}}
		if err := c.call(ctx, "apply", map[string]any{"update": update}, &dataset); err != nil {
			return nil, err
		}
		if dataset.ID != scope {
			return nil, errors.New("retrieval_publication_identity_invalid")
		}
		start = end
		if _, err := e.cacheBytes(); err != nil {
			return nil, err
		}
	}
	if dataset.ID != scope || dataset.Revision < 0 || dataset.Digest == "" {
		return nil, errors.New("retrieval_publication_identity_invalid")
	}
	if _, err := e.cacheBytes(); err != nil {
		return nil, err
	}
	success = true
	return &publication{client: c, directory: dir, content: content, dataset: dataset, ranking: hello.Ranking, generation: generation, used: time.Now()}, nil
}

type client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses chan []byte
	closed    chan struct{}
	stop      chan struct{}
	sequence  int
	once      sync.Once
}
type discardDiagnostics struct{}

func (discardDiagnostics) Write(p []byte) (int, error) { return len(p), nil }
func start(ctx context.Context, config Config, path string) (*client, error) {
	file, err := os.Open(config.Executable)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(config.SHA256) {
		return nil, errors.New("retrieval_binary_digest_mismatch")
	}
	// Execute the verified open inode, closing the path replacement race.
	cmd := exec.Command("/proc/self/fd/3", "serve", "--stdio", "--config", path)
	// A daemon crash must not leave a plaintext index subprocess alive after
	// its cache-directory lease has been released by the kernel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.ExtraFiles = []*os.File{file}
	cmd.Stderr = discardDiagnostics{}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &client{cmd: cmd, stdin: in, responses: make(chan []byte, 1), closed: make(chan struct{}), stop: make(chan struct{})}
	go func() {
		defer close(c.closed)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			line := append([]byte{}, scanner.Bytes()...)
			select {
			case c.responses <- line:
			case <-c.stop:
				return
			}
		}
	}()
	return c, nil
}
func (c *client) close() {
	c.once.Do(func() { close(c.stop); _ = c.stdin.Close(); _ = c.cmd.Process.Kill(); _ = c.cmd.Wait() })
}
func (c *client) call(ctx context.Context, method string, payload map[string]any, result any) error {
	c.sequence++
	id := fmt.Sprintf("skald-%d", c.sequence)
	payload["id"] = id
	payload["method"] = method
	wire, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(wire) > 16<<20 {
		return errors.New("retrieval_request_too_large")
	}
	wrote := make(chan error, 1)
	go func() { _, err := c.stdin.Write(append(wire, '\n')); wrote <- err }()
	select {
	case err := <-wrote:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		c.close()
		return ctx.Err()
	}
	var raw []byte
	select {
	case raw = <-c.responses:
	case <-c.closed:
		return errors.New("retrieval_process_closed")
	case <-ctx.Done():
		c.close()
		return ctx.Err()
	}
	var response struct {
		Version int             `json:"protocol_version"`
		ID      string          `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("retrieval_%s_protocol_fault", method)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("retrieval_%s_protocol_fault", method)
	}
	if response.Version != 1 || response.ID != id || (response.Error == nil) == (len(response.Result) == 0) {
		return fmt.Errorf("retrieval_%s_protocol_fault", method)
	}
	if response.Error != nil {
		if len(response.Error.Code) > 64 {
			return fmt.Errorf("retrieval_%s_protocol_fault", method)
		}
		return fmt.Errorf("braid_%s_%s", method, response.Error.Code)
	}
	return json.Unmarshal(response.Result, result)
}

func (e *Engine) cacheBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(e.config.Directory, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			allocated := info.Size()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				allocated = max(allocated, stat.Blocks*512)
			}
			total += allocated
			if total > e.config.MaxCacheBytes {
				return errors.New("retrieval_cache_too_large")
			}
		}
		return nil
	})
	return total, err
}

// fitContext keeps the ranked prefix whose complete citation-bearing JSON fits.
// Braid's projection excludes our exact-reference fields; its byte bound alone
// cannot account for the final context envelope. Never split a text or citation.
func fitContext(items []Item, maxBytes int) ([]Item, int) {
	used := 2
	count := 0
	for _, item := range items {
		encoded, _ := json.Marshal(item)
		cost := len(encoded)
		if count > 0 {
			cost++
		}
		if cost > maxBytes-used {
			break
		}
		used += cost
		count++
	}
	if count == 0 {
		return []Item{}, 2
	}
	return items[:count], used
}
