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
)

func runDiagnostics(args []string, out, stderr io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	socket := f.String("socket", "", "private daemon socket")
	kind := f.String("kind", "cpu", "cpu or heap profile")
	seconds := f.Int("seconds", 0, "CPU duration, 1–30 seconds (default 10)")
	destination := f.String("output", "", "new private output file")
	if err := f.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if args[0] == "metrics" && v.Name != "socket" {
			invalid = true
		}
	})
	if invalid {
		return errors.New("flag_not_supported_for_command")
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
	if args[0] == "metrics" {
		var response any
		if err := c.Do(ctx, "GET", "/manage/metrics", nil, &response); err != nil {
			return err
		}
		return writeJSON(out, response)
	}
	if *destination == "" {
		return errors.New("profile_output_required")
	}
	if *kind == "cpu" && *seconds == 0 {
		*seconds = 10
	}
	if (*kind != "cpu" && *kind != "heap") || (*kind == "heap" && *seconds != 0) || (*kind == "cpu" && (*seconds < 1 || *seconds > 30)) {
		return errors.New("invalid_profile_options")
	}
	path, err := filepath.Abs(*destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return errors.New("profile_output_exists_or_unavailable")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".skald-profile-*")
	if err != nil {
		return errors.New("profile_output_unavailable")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := c.Profile(ctx, *kind, *seconds, file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return errors.New("profile_output_exists_or_unavailable")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	return writeJSON(out, map[string]any{"profile": path, "kind": *kind, "seconds": *seconds})
}
