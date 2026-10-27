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

	"github.com/algorhythmic/skald/internal/daemon"
)

func runContext(args []string, out, stderr io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	socket := f.String("socket", "", "private daemon socket")
	ns := f.String("namespace", "", "configured namespace scope")
	conversation := f.String("conversation", "", "conversation key")
	text := f.String("text", "", "lexical query text")
	maxBytes := f.Int("max-bytes", 16384, "maximum context JSON bytes (2..65536)")
	rows := f.Int("max-rows", 100, "maximum payload rows per maintenance transaction")
	input := f.Int("max-input-bytes", 4<<20, "maximum logical payload input bytes per transaction")
	if err := f.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	allowed := map[string]map[string]bool{"descriptions": {"socket": true, "namespace": true, "conversation": true}, "context": {"socket": true, "namespace": true, "text": true, "max-bytes": true}, "compress": {"socket": true, "max-rows": true, "max-input-bytes": true}}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			invalid = true
		}
	})
	if invalid {
		return errors.New("flag_not_supported_for_command")
	}
	if *socket == "" {
		p, e := daemon.DefaultSocket()
		if e != nil {
			return e
		}
		*socket = p
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("absolute_socket_path_required")
	}
	q := url.Values{}
	if *ns != "" {
		q.Set("namespace", *ns)
	}
	method, path := "GET", "/v1/"+args[0]
	switch args[0] {
	case "context":
		q.Set("text", *text)
		q.Set("max_bytes", strconv.Itoa(*maxBytes))
	case "descriptions":
		if *conversation == "" {
			return errors.New("conversation_required")
		}
		q.Set("conversation", *conversation)
	case "compress":
		method, path = "POST", "/manage/compress"
		q.Set("max_rows", strconv.Itoa(*rows))
		q.Set("max_input_bytes", strconv.Itoa(*input))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	value, err := request(ctx, *socket, method, path, q)
	if err != nil {
		return err
	}
	return writeJSON(out, value)
}
