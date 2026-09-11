package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/client"
	"github.com/algorhythmic/skald/internal/daemon"
	"github.com/algorhythmic/skald/internal/tui"
)

func runTUI(args []string, stderr io.Writer, ensure bool) error {
	f := flag.NewFlagSet("tui", flag.ContinueOnError)
	f.SetOutput(stderr)
	defaultTheme := os.Getenv("SKALD_THEME")
	if defaultTheme == "" {
		defaultTheme = string(tui.ThemeHeimdall)
	}
	themeName := f.String("theme", defaultTheme, "colors: heimdall (default), desktop (terminal palette), or amber")
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
	if ensure {
		ensureDaemon(ctx, *socket, stderr)
	}
	c := client.New(*socket)
	defer c.Close()
	return tui.Run(ctx, tui.Remote{Client: c, Namespace: *ns}, theme)
}

// ensureDaemon spawns a detached archive daemon when no peer answers the
// socket. The daemon is independent: it keeps running after the TUI exits.
func ensureDaemon(ctx context.Context, socket string, stderr io.Writer) {
	probe, stop := context.WithTimeout(ctx, 2*time.Second)
	c := client.New(socket)
	var status any
	err := c.Do(probe, "GET", "/v1/status", nil, &status)
	stop()
	c.Close()
	if err == nil || err.Error() != "daemon_unavailable" {
		return
	}
	cfg, cfgErr := daemon.DefaultConfigPath()
	if cfgErr != nil {
		return
	}
	if info, statErr := os.Stat(cfg); statErr != nil || info.IsDir() {
		fmt.Fprintln(stderr, "skald: daemon not running; connect a source first with `skald connect`")
		return
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(socket), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log = nil
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "serve", "--config", cfg, "--socket", socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if log != nil {
		cmd.Stderr = log
	}
	if err := cmd.Start(); err != nil {
		if log != nil {
			log.Close()
		}
		fmt.Fprintf(stderr, "skald: daemon start failed: %v\n", err)
		return
	}
	go func() {
		_ = cmd.Wait()
		if log != nil {
			log.Close()
		}
	}()
	fmt.Fprintln(stderr, "skald: daemon started in background; connecting")
}
