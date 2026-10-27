package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
)

// BackupConfig has no default destination. OffDevice is an operator declaration;
// it is additionally checked against mount and physical block-device evidence.
type BackupConfig struct {
	Destination string `json:"destination"`
	OffDevice   bool   `json:"off_device"`
}

func (c BackupConfig) Validate() error {
	if c.Destination == "" {
		if c.OffDevice {
			return errors.New("backup_destination_required")
		}
		return nil
	}
	_, e := safe(c.Destination, true)
	return e
}

type BackupState struct {
	Status                        string     `json:"status"`
	Configured                    bool       `json:"configured"`
	DestinationVerifiedOffDevice  bool       `json:"destination_verified_off_device"`
	LastOffDeviceBackup           *time.Time `json:"last_off_device_backup"`
	LastOffDeviceBackupAgeSeconds *int64     `json:"last_off_device_backup_age_seconds"`
	LastVerifiedRestore           *time.Time `json:"last_verified_restore"`
	LastVerifiedRestoreAgeSeconds *int64     `json:"last_verified_restore_age_seconds"`
	OffDeviceRecoveryVerified     bool       `json:"off_device_recovery_verified"`
	L1Complete                    bool       `json:"l1_complete"`
}
type backupReceipt struct {
	LastOffDeviceBackup *time.Time `json:"last_off_device_backup,omitempty"`
	Version             int        `json:"version"`
	ArchiveInstance     string     `json:"archive_instance"`
	Destination         string     `json:"destination"`
	SourceDevice        uint64     `json:"source_device"`
	DestinationDevice   uint64     `json:"destination_device"`
	OffDevice           bool       `json:"off_device_verified"`
	BackupAt            time.Time  `json:"backup_at"`
	RestoreAt           time.Time  `json:"restore_at"`
	CompressedSHA256    string     `json:"compressed_sha256"`
}

func device(path string) (uint64, error) {
	i, e := os.Stat(path)
	if e != nil {
		return 0, e
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("device_identity_unavailable")
	}
	return uint64(s.Dev), nil
}
func majorMinor(dev uint64) (uint64, uint64) {
	return (dev>>8)&0xfff | (dev>>32)&0xfffff000, dev&0xff | (dev>>12)&0xffffff00
}
func physicalDevices(path string, seen map[string]bool) (map[string]bool, error) {
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return nil, e
	}
	if seen[resolved] {
		return nil, errors.New("device_cycle")
	}
	seen[resolved] = true
	if _, e = os.Stat(filepath.Join(resolved, "partition")); e == nil {
		resolved = filepath.Dir(resolved)
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	slaves, e := os.ReadDir(filepath.Join(resolved, "slaves"))
	if e != nil {
		return nil, e
	}
	result := map[string]bool{}
	for _, slave := range slaves {
		children, e := physicalDevices(filepath.Join(resolved, "slaves", slave.Name()), seen)
		if e != nil {
			return nil, e
		}
		for k := range children {
			result[k] = true
		}
	}
	if len(slaves) == 0 {
		physical, e := filepath.EvalSymlinks(filepath.Join(resolved, "device"))
		if e != nil {
			return nil, e
		}
		result[physical] = true
	}
	return result, nil
}
func networkMount(path string) bool {
	data, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return false
	}
	best := ""
	remote := false
	unescape := func(s string) string {
		r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
		return r.Replace(s)
	}
	for _, line := range strings.Split(string(data), "\n") {
		halves := strings.SplitN(line, " - ", 2)
		if len(halves) != 2 {
			continue
		}
		left, right := strings.Fields(halves[0]), strings.Fields(halves[1])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		mount := unescape(left[4])
		if path != mount && !strings.HasPrefix(path, strings.TrimSuffix(mount, "/")+"/") {
			continue
		}
		if len(mount) < len(best) {
			continue
		}
		best = mount
		switch right[0] {
		case "nfs", "nfs4", "cifs", "smb3", "fuse.sshfs":
			remote = true
		default:
			remote = false
		}
	}
	return remote
}

// independentDevices fails closed for overlay/tmpfs/loop/unknown devices and
// partitions sharing any physical backing device. Known remote mounts only count
// when the archive itself is local, and the operator explicitly opted in.
func independentDevices(source, destination string, declared bool) (bool, uint64, uint64, error) {
	src, e := device(source)
	if e != nil {
		return false, 0, 0, e
	}
	dst, e := device(destination)
	if e != nil {
		return false, src, 0, e
	}
	if !declared || src == dst {
		return false, src, dst, nil
	}
	if networkMount(destination) && !networkMount(source) {
		return true, src, dst, nil
	}
	a, b := majorMinor(src)
	sourceDevices, e := physicalDevices(fmt.Sprintf("/sys/dev/block/%d:%d", a, b), map[string]bool{})
	if e != nil {
		return false, src, dst, nil
	}
	a, b = majorMinor(dst)
	destinationDevices, e := physicalDevices(fmt.Sprintf("/sys/dev/block/%d:%d", a, b), map[string]bool{})
	if e != nil {
		return false, src, dst, nil
	}
	for k := range destinationDevices {
		if sourceDevices[k] {
			return false, src, dst, nil
		}
	}
	return len(sourceDevices) > 0 && len(destinationDevices) > 0, src, dst, nil
}
func sourceArchiveDir(source string) (string, error) {
	dir := filepath.Dir(source)
	for i := 0; i < 2; i++ {
		if _, e := os.Lstat(filepath.Join(dir, "archive.sqlite")); e == nil {
			if _, e = safe(dir, true); e != nil {
				return "", e
			}
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("source_archive_directory_unavailable")
}
func saveBackupReceipt(dir string, receipt backupReceipt) error {
	lock, e := directoryLock(context.Background(), dir, true)
	if e != nil {
		return e
	}
	defer unlock(lock)
	path := filepath.Join(dir, "backup-status.json")
	if _, e = safe(path, false); e != nil && !os.IsNotExist(e) {
		return e
	}
	if previous, e := loadBackupReceipt(dir); e == nil && previous.ArchiveInstance == receipt.ArchiveInstance {
		receipt.LastOffDeviceBackup = previous.LastOffDeviceBackup
		if receipt.LastOffDeviceBackup == nil && previous.OffDevice {
			receipt.LastOffDeviceBackup = &previous.BackupAt
		}
	}
	if receipt.OffDevice {
		receipt.LastOffDeviceBackup = &receipt.BackupAt
	}
	f, e := os.CreateTemp(dir, ".backup-status-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = json.NewEncoder(f).Encode(receipt); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return lock.Sync()
}

// BackupStatus reports only a matching durable receipt after successful export
// and fresh restore. Paths and device identifiers stay in the private receipt,
// never in this status projection or telemetry. L1 remains a broader open gate.
func loadBackupReceipt(archiveDir string) (backupReceipt, error) {
	var r backupReceipt
	path := filepath.Join(archiveDir, "backup-status.json")
	i, e := safe(path, false)
	if e != nil {
		return r, e
	}
	if i.Size() > 16384 {
		return r, errors.New("backup_status_too_large")
	}
	f, e := os.Open(path)
	if e != nil {
		return r, e
	}
	defer f.Close()
	wire, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil {
		return r, e
	}
	if len(wire) > 16384 {
		return r, errors.New("backup_status_too_large")
	}
	e = json.Unmarshal(wire, &r)
	return r, e
}
func BackupStatus(archiveDir string, cfg BackupConfig, archiveInstance string) (BackupState, error) {
	out := BackupState{Status: "no off-device backup configured", Configured: cfg.Destination != ""}
	receipt, receiptErr := loadBackupReceipt(archiveDir)
	if receiptErr != nil && !os.IsNotExist(receiptErr) {
		return out, receiptErr
	}
	matches := receiptErr == nil && receipt.Version == 1 && receipt.ArchiveInstance == archiveInstance && !receipt.BackupAt.IsZero() && !receipt.RestoreAt.Before(receipt.BackupAt)
	age := func(t time.Time) *int64 {
		seconds := int64(time.Since(t).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		return &seconds
	}
	if matches {
		out.LastVerifiedRestore = &receipt.RestoreAt
		out.LastVerifiedRestoreAgeSeconds = age(receipt.RestoreAt)
		offAt := receipt.LastOffDeviceBackup
		if offAt == nil && receipt.OffDevice {
			offAt = &receipt.BackupAt
		}
		if offAt != nil && !offAt.IsZero() {
			out.LastOffDeviceBackup = offAt
			out.LastOffDeviceBackupAgeSeconds = age(*offAt)
		}
	}
	if cfg.Destination == "" {
		return out, nil
	}
	if e := cfg.Validate(); e != nil {
		out.Status = "off-device backup destination unverified"
		return out, nil
	}
	off, src, dst, e := independentDevices(archiveDir, cfg.Destination, cfg.OffDevice)
	if e != nil {
		out.Status = "off-device backup destination unverified"
		return out, nil
	}
	if !off {
		out.Status = "off-device backup destination unverified"
		return out, nil
	}
	out.DestinationVerifiedOffDevice = true
	out.Status = "off-device backup configured; no verified recovery receipt"
	if matches && receipt.Destination == filepath.Clean(cfg.Destination) && receipt.SourceDevice == src && receipt.DestinationDevice == dst && receipt.OffDevice {
		out.OffDeviceRecoveryVerified = true
		out.Status = "off-device backup and fresh restore verified"
	}
	return out, nil
}

// ExportBackupConfigured records success only after compression and fresh restore.
// A local destination may be used, but cannot establish off-device recovery.
func ExportBackupConfigured(ctx context.Context, source string, cfg BackupConfig) (archive.BackupManifest, error) {
	var empty archive.BackupManifest
	if e := cfg.Validate(); e != nil {
		return empty, e
	}
	if cfg.Destination == "" {
		return empty, errors.New("backup_destination_required")
	}
	dir, e := sourceArchiveDir(source)
	if e != nil {
		return empty, e
	}
	off, src, dst, e := independentDevices(dir, cfg.Destination, cfg.OffDevice)
	if e != nil {
		return empty, e
	}
	start := time.Now().UTC()
	manifest, e := exportBackup(ctx, source, cfg.Destination)
	if e != nil {
		return empty, e
	}
	receipt := backupReceipt{Version: 1, ArchiveInstance: manifest.ArchiveInstance, Destination: filepath.Clean(cfg.Destination), SourceDevice: src, DestinationDevice: dst, OffDevice: off, BackupAt: start, RestoreAt: time.Now().UTC(), CompressedSHA256: manifest.CompressedSHA256}
	if e = saveBackupReceipt(dir, receipt); e != nil {
		return manifest, e
	}
	return manifest, nil
}
