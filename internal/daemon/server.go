package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
)

type API struct {
	Store     *archive.Store
	Collector *Collector
	Config    Config
	gate      sync.RWMutex
	stopped   bool
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.gate.RLock()
	defer a.gate.RUnlock()
	if a.stopped {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if len(r.URL.RawQuery) > 8192 {
		respond(w, nil, errors.New("invalid_request"))
		return
	}
	ns, err := archive.Scope(a.Config.Namespaces(), r.URL.Query().Get("namespace"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/manage/backup" {
		if len(r.URL.RawQuery) != 0 || r.ContentLength != 0 {
			respond(w, nil, errors.New("invalid_request"))
			return
		}
		path, err := a.Store.Backup(r.Context())
		respond(w, map[string]string{"backup": path}, err)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	allowed := map[string]map[string]bool{
		"/v1/status":     {"namespace": true},
		"/v1/sessions":   {"namespace": true, "limit": true, "cursor": true},
		"/v1/transcript": {"namespace": true, "limit": true, "cursor": true, "conversation": true, "history": true},
		"/v1/artifacts":  {"namespace": true, "limit": true, "cursor": true, "conversation": true},
		"/v1/record":     {"namespace": true, "record": true, "revision": true, "adapter": true, "raw": true},
	}
	for k, values := range q {
		if !allowed[r.URL.Path][k] || len(values) != 1 {
			respond(w, nil, errors.New("invalid_request"))
			return
		}
	}
	limit := 25
	if v := q.Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil {
			respond(w, nil, errors.New("invalid_page_limit"))
			return
		}
	}
	switch r.URL.Path {
	case "/v1/status":
		status, err := a.Store.Status(r.Context(), ns)
		if err == nil {
			issues := a.Collector.Issues()
			visible := map[string]string{}
			for _, source := range a.Collector.Sources() {
				for _, n := range ns {
					if source.Namespace == n {
						if code, ok := issues[source.Key()]; ok {
							visible[source.Key()] = code
						}
					}
				}
			}
			status["capture_issues"] = visible
			status["allow_raw"] = a.Config.AllowRaw
			discovery := []DiscoveryStatus{}
			for _, root := range a.Collector.DiscoveryStatus() {
				for _, n := range ns {
					if root.Namespace == n {
						discovery = append(discovery, root)
					}
				}
			}
			status["discovery"] = discovery
		}
		respond(w, status, err)
	case "/v1/sessions":
		result, err := a.Store.Sessions(r.Context(), ns, q.Get("cursor"), limit)
		respond(w, result, err)
	case "/v1/artifacts":
		result, err := a.Store.Artifacts(r.Context(), ns, q.Get("conversation"), q.Get("cursor"), limit)
		respond(w, result, err)
	case "/v1/transcript":
		if q.Get("history") != "" && q.Get("history") != "true" && q.Get("history") != "false" {
			respond(w, nil, errors.New("invalid_request"))
			return
		}
		result, err := a.Store.Transcript(r.Context(), ns, q.Get("conversation"), q.Get("cursor"), limit, q.Get("history") == "true")
		respond(w, result, err)
	case "/v1/record":
		if q.Get("raw") != "" && q.Get("raw") != "true" && q.Get("raw") != "false" {
			respond(w, nil, errors.New("invalid_request"))
			return
		}
		raw := q.Get("raw") == "true"
		if raw && !a.Config.AllowRaw {
			respond(w, nil, errors.New("scope_denied"))
			return
		}
		result, err := a.Store.Get(r.Context(), ns, q.Get("record"), q.Get("revision"), q.Get("adapter"), raw)
		respond(w, result, err)
	default:
		respond(w, nil, errors.New("not_found"))
	}
}
func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		code := archive.ErrorCode(err)
		status := http.StatusBadRequest
		if code == "scope_denied" {
			status = http.StatusForbidden
		}
		if code == "cursor_expired" {
			status = http.StatusConflict
		}
		if code == "archive_error" {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
		return
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > archive.MaxResponseBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":"response_too_large"}`)
		return
	}
	_, _ = w.Write(body)
}

// sameUserListener checks SO_PEERCRED in addition to private directory/socket
// permissions. Rejected peers receive no HTTP parser or archive access.
type sameUserListener struct{ net.Listener }

func (l sameUserListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		unix, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			continue
		}
		raw, err := unix.SyscallConn()
		if err != nil {
			conn.Close()
			continue
		}
		accepted := false
		err = raw.Control(func(fd uintptr) {
			cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
			accepted = e == nil && cred.Uid == uint32(os.Getuid())
		})
		if err == nil && accepted {
			return conn, nil
		}
		conn.Close()
	}
}

// Serve runs in the foreground. Cancellation stops collection and HTTP before
// closing SQLite. No user services or provider hooks are installed.
func Serve(ctx context.Context, cfg Config, dataDir, socket string, ready func()) error {
	s, err := archive.Open(dataDir, cfg.Options())
	if err != nil {
		return err
	}
	defer s.Close()
	collector := &Collector{Store: s, Config: cfg}
	if err := collector.Initialize(ctx); err != nil {
		return err
	}
	parent := filepath.Dir(socket)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("private_socket_directory_required")
	}
	// A separate socket lock protects endpoints even when archives differ.
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("socket_in_use")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("socket_path_exists")
		}
		if live, e := net.DialTimeout("unix", socket, 200*time.Millisecond); e == nil {
			live.Close()
			return errors.New("socket_in_use")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	collectDone := make(chan struct{})
	go func() { defer close(collectDone); collector.Run(runCtx) }()
	api := &API{Store: s, Collector: collector, Config: cfg}
	server := &http.Server{Handler: api, BaseContext: func(net.Listener) context.Context { return runCtx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(sameUserListener{listener}) }()
	if ready != nil {
		ready()
	}
	select {
	case <-ctx.Done():
		err = nil
	case err = <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	cancel()
	shutdownCtx, end := context.WithTimeout(context.Background(), 5*time.Second)
	defer end()
	if e := server.Shutdown(shutdownCtx); e != nil {
		_ = server.Close()
	}
	api.gate.Lock()
	api.stopped = true
	api.gate.Unlock()
	<-collectDone
	return err
}
