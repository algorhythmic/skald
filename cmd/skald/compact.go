package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/algorhythmic/skald/internal/archive"
)

func runArchiveCompact(args []string, out, stderr io.Writer) error {
	f := flag.NewFlagSet("archive-compact", flag.ContinueOnError)
	f.SetOutput(stderr)
	dir := f.String("data-dir", "", "private archive directory (required)")
	apply := f.Bool("apply", false, "reclaim database pages; requires stopped daemon")
	options := archive.DefaultOptions()
	max := f.Int64("max-database-bytes", options.MaxDatabaseBytes, "configured archive database capacity")
	min := f.Uint64("min-free-bytes", options.MinFreeBytes, "required free filesystem headroom after temporary copies")
	if err := f.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	if *dir == "" {
		return errors.New("data_directory_required")
	}
	options.MaxDatabaseBytes = *max
	options.MinFreeBytes = *min
	if !*apply {
		preview, err := archive.PreviewCompaction(*dir, options)
		if err != nil {
			return err
		}
		return writeJSON(out, archive.CompactionResult{Before: preview})
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := archive.CompactOffline(ctx, *dir, options)
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}
