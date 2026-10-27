package maintenance

import (
	"context"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/archive"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNoOffDeviceDestinationAndSameDisk(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	state, e := BackupStatus(dir, BackupConfig{}, "instance")
	if e != nil {
		t.Fatal(e)
	}
	if state.Status != "no off-device backup configured" || state.L1Complete || state.LastOffDeviceBackup != nil || state.LastVerifiedRestore != nil {
		t.Fatal(state)
	}
	destination := t.TempDir()
	os.Chmod(destination, 0700)
	state, e = BackupStatus(dir, BackupConfig{Destination: destination, OffDevice: true}, "instance")
	if e != nil {
		t.Fatal(e)
	}
	if state.DestinationVerifiedOffDevice || state.OffDeviceRecoveryVerified || state.LastOffDeviceBackup != nil || state.Status != "off-device backup destination unverified" {
		t.Fatal("same disk was counted independent", state)
	}
	if e = (BackupConfig{OffDevice: true}).Validate(); e == nil {
		t.Fatal("default destination inferred")
	}
}
func TestLocalExportReceiptNeverEstablishesOffDeviceRecovery(t *testing.T) {
	ctx := context.Background()
	s, e := archive.Open(filepath.Join(t.TempDir(), "live"), archive.DefaultOptions())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	source, e := s.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	os.Chmod(dest, 0700)
	manifest, e := ExportBackupConfigured(ctx, source, BackupConfig{Destination: dest, OffDevice: true})
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Dir(filepath.Dir(source))
	wire, e := os.ReadFile(filepath.Join(dir, "backup-status.json"))
	if e != nil {
		t.Fatal(e)
	}
	var receipt backupReceipt
	if e = json.Unmarshal(wire, &receipt); e != nil {
		t.Fatal(e)
	}
	if receipt.OffDevice || receipt.ArchiveInstance != manifest.ArchiveInstance || receipt.RestoreAt.Before(receipt.BackupAt) || receipt.SourceDevice != receipt.DestinationDevice {
		t.Fatal(receipt)
	}
	state, e := BackupStatus(dir, BackupConfig{Destination: dest, OffDevice: true}, manifest.ArchiveInstance)
	if e != nil {
		t.Fatal(e)
	}
	if state.OffDeviceRecoveryVerified || state.LastVerifiedRestore == nil || state.LastOffDeviceBackup != nil || state.L1Complete {
		t.Fatal("local receipt claimed independent recovery", state)
	}
	if _, e = ExportBackupConfigured(ctx, source, BackupConfig{Destination: dest, OffDevice: true}); e == nil {
		t.Fatal("overwrite accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "backup-status.json"))
	if string(after) != string(wire) {
		t.Fatal("failed export rewrote receipt")
	}
}
func TestPhysicalPartitionsShareDeviceIdentity(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "block", "sda")
	hardware := filepath.Join(root, "hardware", "disk-one")
	os.MkdirAll(filepath.Join(disk, "slaves"), 0700)
	os.MkdirAll(hardware, 0700)
	os.Symlink(hardware, filepath.Join(disk, "device"))
	for _, name := range []string{"sda1", "sda2"} {
		part := filepath.Join(disk, name)
		os.Mkdir(part, 0700)
		os.WriteFile(filepath.Join(part, "partition"), []byte("1"), 0600)
	}
	first, e := physicalDevices(filepath.Join(disk, "sda1"), map[string]bool{})
	if e != nil {
		t.Fatal(e)
	}
	second, e := physicalDevices(filepath.Join(disk, "sda2"), map[string]bool{})
	if e != nil {
		t.Fatal(e)
	}
	if len(first) != 1 || len(second) != 1 || !first[hardware] || !second[hardware] {
		t.Fatal("partition identity differs", first, second)
	}
	virtual := filepath.Join(root, "block", "dm-0")
	os.MkdirAll(filepath.Join(virtual, "slaves"), 0700)
	os.Symlink(filepath.Join(disk, "sda1"), filepath.Join(virtual, "slaves", "sda1"))
	mapped, e := physicalDevices(virtual, map[string]bool{})
	if e != nil || !mapped[hardware] {
		t.Fatal("mapped device lost physical ancestry", mapped, e)
	}
	if _, e = physicalDevices(filepath.Join(root, "unknown"), map[string]bool{}); e == nil {
		t.Fatal("unknown device established proof")
	}
}
func TestLinuxDeviceNumbers(t *testing.T) {
	major, minor := majorMinor(8<<8 | 17)
	if major != 8 || minor != 17 {
		t.Fatal(major, minor)
	}
}

func TestHistoricalAgesSurviveUnsetOfflineAndLocalExport(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	old := time.Now().UTC().Add(-2 * time.Hour)
	r := backupReceipt{Version: 1, ArchiveInstance: "instance", Destination: "/synthetic/off-device", OffDevice: true, BackupAt: old, RestoreAt: old.Add(time.Minute)}
	if e := saveBackupReceipt(dir, r); e != nil {
		t.Fatal(e)
	}
	for _, cfg := range []BackupConfig{{}, {Destination: "/unavailable/synthetic", OffDevice: true}} {
		state, e := BackupStatus(dir, cfg, "instance")
		if e != nil {
			t.Fatal(e)
		}
		if state.LastOffDeviceBackup == nil || state.LastVerifiedRestore == nil || state.LastOffDeviceBackupAgeSeconds == nil || state.OffDeviceRecoveryVerified || state.L1Complete {
			t.Fatal(state)
		}
		if cfg.Destination == "" && state.Status != "no off-device backup configured" {
			t.Fatal(state)
		}
	}
	local := backupReceipt{Version: 1, ArchiveInstance: "instance", Destination: "/synthetic/local", BackupAt: old.Add(time.Hour), RestoreAt: old.Add(time.Hour + time.Minute)}
	if e := saveBackupReceipt(dir, local); e != nil {
		t.Fatal(e)
	}
	state, e := BackupStatus(dir, BackupConfig{}, "instance")
	if e != nil {
		t.Fatal(e)
	}
	if state.LastOffDeviceBackup == nil || !state.LastOffDeviceBackup.Equal(old) || state.LastVerifiedRestore == nil || !state.LastVerifiedRestore.Equal(local.RestoreAt) {
		t.Fatal("historical off-device receipt lost", state)
	}
	other, e := BackupStatus(dir, BackupConfig{}, "restored-instance")
	if e != nil || other.LastOffDeviceBackup != nil || other.LastVerifiedRestore != nil {
		t.Fatal("receipt crossed archive instance", other, e)
	}
}
