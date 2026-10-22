package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/algorhythmic/skald/internal/mcp"
)

type namespaceGrants []string

func (g *namespaceGrants) String() string { return "explicit namespace grant" }
func (g *namespaceGrants) Set(value string) error {
	if value == "" {
		return errors.New("namespace_required")
	}
	*g = append(*g, value)
	return nil
}

func runMCP(args []string, in io.Reader, out, stderr io.Writer) error {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	f.SetOutput(stderr)
	socket := f.String("socket", "", "absolute path to an existing Skald daemon socket")
	var namespaces namespaceGrants
	f.Var(&namespaces, "namespace", "explicit namespace grant; repeat to grant several")
	allowRaw := f.Bool("allow-raw", false, "expose raw exact-record reads (daemon must also allow raw)")
	if err := f.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("absolute_socket_path_required")
	}
	s, err := mcp.New(mcp.Config{Socket: *socket, Namespaces: namespaces, AllowRaw: *allowRaw})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return s.Serve(ctx, in, out)
}
