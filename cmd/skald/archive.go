package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/client"
	"github.com/algorhythmic/skald/internal/daemon"
)

func isArchiveCommand(cmd string) bool {
	switch cmd {
	case "serve", "status", "sessions", "artifacts", "get", "backup", "restore":
		return true
	}
	return false
}
func runArchive(args []string, out, stderr io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "explicit source configuration")
	data := f.String("data-dir", "", "private archive directory")
	socket := f.String("socket", "", "private daemon socket")
	ns := f.String("namespace", "", "narrow to a configured namespace")
	limit := f.Int("limit", 25, "maximum returned references")
	cursor := f.String("cursor", "", "page cursor")
	conversation := f.String("conversation", "", "conversation key")
	record := f.String("record", "", "record key")
	revision := f.String("revision", "", "exact source revision")
	adapter := f.String("adapter", "", "exact normalization version")
	raw := f.Bool("raw", false, "request source bytes; requires allow_raw in daemon config")
	backup := f.String("backup", "", "verified archive backup file")
	if err := f.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	allowed := map[string]map[string]bool{
		"serve":     {"config": true, "data-dir": true, "socket": true},
		"status":    {"socket": true, "namespace": true},
		"sessions":  {"socket": true, "namespace": true, "limit": true, "cursor": true},
		"artifacts": {"socket": true, "namespace": true, "limit": true, "cursor": true, "conversation": true},
		"get":       {"socket": true, "namespace": true, "record": true, "revision": true, "adapter": true, "raw": true},
		"backup":    {"socket": true}, "restore": {"backup": true, "data-dir": true},
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			invalid = true
		}
	})
	if invalid {
		return errors.New("flag_not_supported_for_command")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "restore" {
		if *data == "" || *backup == "" {
			return errors.New("backup_and_fresh_data_directory_required")
		}
		if err := archive.Restore(ctx, *backup, *data, archive.DefaultOptions()); err != nil {
			return err
		}
		return writeJSON(out, map[string]string{"restored_to": *data})
	}
	if *socket == "" {
		path, err := daemon.DefaultSocket()
		if err != nil {
			return err
		}
		*socket = path
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("absolute_socket_path_required")
	}
	if args[0] == "serve" {
		if *config == "" {
			p, err := daemon.DefaultConfigPath()
			if err != nil {
				return err
			}
			*config = p
		}
		cfg, err := daemon.LoadConfig(*config)
		if err != nil {
			return err
		}
		if *data == "" {
			path, err := daemon.DefaultDataDir()
			if err != nil {
				return err
			}
			*data = path
		}
		return daemon.Serve(ctx, cfg, *data, *socket, func() {
			_ = writeJSON(out, map[string]string{"daemon": "ready", "socket": *socket, "initial_capture": "pending"})
		})
	}
	q := url.Values{}
	if *ns != "" {
		q.Set("namespace", *ns)
	}
	endpoint := "/v1/" + args[0]
	method := "GET"
	switch args[0] {
	case "sessions", "artifacts":
		q.Set("limit", strconv.Itoa(*limit))
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
		if args[0] == "artifacts" {
			if *conversation == "" {
				return errors.New("conversation_required")
			}
			q.Set("conversation", *conversation)
		}
	case "get":
		if *record == "" || *revision == "" || *adapter == "" {
			return errors.New("exact_record_revision_and_adapter_required")
		}
		endpoint = "/v1/record"
		q.Set("record", *record)
		q.Set("revision", *revision)
		q.Set("adapter", *adapter)
		if *raw {
			q.Set("raw", "true")
		}
	case "backup":
		endpoint = "/manage/backup"
		method = "POST"
	}
	response, err := request(ctx, *socket, method, endpoint, q)
	if err != nil {
		return err
	}
	return writeJSON(out, response)
}
func request(ctx context.Context, socket, method, path string, q url.Values) (any, error) {
	c := client.New(socket)
	defer c.Close()
	var value any
	err := c.Do(ctx, method, path, q, &value)
	return value, err
}
