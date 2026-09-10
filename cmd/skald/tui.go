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

	"github.com/algorhythmic/skald/internal/client"
	"github.com/algorhythmic/skald/internal/daemon"
	"github.com/algorhythmic/skald/internal/tui"
)

func runTUI(args []string, stderr io.Writer) error {
	f := flag.NewFlagSet("tui", flag.ContinueOnError)
	f.SetOutput(stderr)
	defaultTheme := os.Getenv("SKALD_THEME")
	if defaultTheme == "" {
		defaultTheme = string(tui.ThemeDesktop)
	}
	themeName := f.String("theme", defaultTheme, "colors: desktop (terminal palette) or amber")
	socket := f.String("socket", "", "private daemon socket")
	ns := f.String("namespace", "", "narrow to a configured namespace")
	if err := f.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	theme, err := tui.ParseTheme(*themeName)
	if err != nil {
		return err
	}
	if *socket == "" {
		p, err := daemon.DefaultSocket()
		if err != nil {
			return err
		}
		*socket = p
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("absolute_socket_path_required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c := client.New(*socket)
	defer c.Close()
	return tui.Run(ctx, tui.Remote{Client: c, Namespace: *ns}, theme)
}
