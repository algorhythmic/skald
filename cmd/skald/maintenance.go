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
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/daemon"
	"github.com/algorhythmic/skald/internal/maintenance"
)

func isMaintenanceCommand(cmd string) bool {
	return cmd == "recovery-retain" || cmd == "recovery-check" || cmd == "recovery-job" || cmd == "storage-usage"
}
func runMaintenance(args []string, out, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("maintenance_command_required")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	dir := f.String("recovery-dir", "", "explicit private recovery directory")
	backup := f.String("backup", "", "sealed backup to verify")
	destination := f.String("destination-dir", "", "independent existing private destination directory")
	socket := f.String("socket", "", "daemon socket")
	keep := f.Int("keep-raw", 1, "minimum newest raw rollback files to preserve")
	age := f.Duration("max-age", 0, "retire redundant raw older than duration; zero uses count")
	bytes := f.Int64("max-bytes", 0, "desired allocated recovery bytes; reports unmet budgets")
	offDevice := f.Bool("off-device", false, "declare destination independent; device evidence must also verify")
	apply := f.Bool("apply", false, "explicitly execute retention or backup job; default is preview")
	if e := f.Parse(args[1:]); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	allowed := map[string]map[string]bool{"recovery-retain": {"recovery-dir": true, "keep-raw": true, "max-age": true, "max-bytes": true, "apply": true}, "recovery-check": {"backup": true}, "recovery-job": {"destination-dir": true, "socket": true, "apply": true, "off-device": true}, "storage-usage": {}}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			invalid = true
		}
	})
	if invalid {
		return errors.New("flag_not_supported_for_command")
	}
	if args[0] != "storage-usage" && f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	switch args[0] {
	case "storage-usage":
		if f.NArg() == 0 {
			return errors.New("explicit_storage_roots_required")
		}
		value, e := maintenance.Usage(f.Args())
		if e != nil {
			return e
		}
		return writeJSON(out, value)
	case "recovery-check":
		if e := maintenance.Verify(ctx, *backup); e != nil {
			return e
		}
		return writeJSON(out, map[string]string{"verified": *backup})
	case "recovery-retain":
		p := maintenance.Policy{KeepRaw: *keep, MaxAge: *age, MaxBytes: *bytes}
		fn := maintenance.PlanRetention
		if *apply {
			fn = maintenance.ApplyRetention
		}
		plan, e := fn(ctx, *dir, p, time.Now())
		if e != nil {
			return e
		}
		return writeJSON(out, struct {
			Applied bool             `json:"applied"`
			Plan    maintenance.Plan `json:"plan"`
		}{*apply, plan})
	case "recovery-job":
		if *destination == "" {
			return errors.New("independent_destination_required")
		}
		if !*apply {
			return writeJSON(out, map[string]any{"applied": false, "destination": *destination, "operation": "daemon backup, compress export, fresh restore verification"})
		}
		// Refuse missing/unmounted or nonprivate destinations before asking the
		// daemon to allocate a new local recovery copy.
		if e := (maintenance.BackupConfig{Destination: *destination, OffDevice: *offDevice}).Validate(); e != nil {
			return e
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
		response, e := request(ctx, *socket, "POST", "/manage/backup", url.Values{})
		if e != nil {
			return e
		}
		m, ok := response.(map[string]any)
		if !ok {
			return errors.New("invalid_backup_response")
		}
		path, ok := m["backup"].(string)
		if !ok {
			return errors.New("invalid_backup_response")
		}
		manifest, e := maintenance.ExportBackupConfigured(ctx, path, maintenance.BackupConfig{Destination: *destination, OffDevice: *offDevice})
		if e != nil {
			return e
		}
		return writeJSON(out, manifest)
	}
	return errors.New("unknown_maintenance_command")
}
