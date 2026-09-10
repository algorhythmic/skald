package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func DefaultConfig() Config {
	return Config{Version: 1, PollMilliseconds: 1000, MaxDatabaseBytes: 4 << 30, MinFreeBytes: 256 << 20}
}
func DefaultConfigPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("absolute_XDG_CONFIG_HOME_required")
	}
	return filepath.Join(base, "skald", "sources.json"), nil
}

// UpdateConfig serializes cooperating enrollment edits and atomically publishes
// a validated private file. A symlink is never followed or overwritten.
func UpdateConfig(path string, edit func(*Config) error) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("private_config_lock_required")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	cfg := DefaultConfig()
	original, err := os.Lstat(path)
	if err == nil {
		if !original.Mode().IsRegular() {
			return errors.New("regular_configuration_required")
		}
		cfg, err = LoadConfig(path)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := edit(&cfg); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".skald-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := json.NewEncoder(tmp).Encode(cfg); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := LoadConfig(tmp.Name()); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if original == nil {
		if err == nil || !os.IsNotExist(err) {
			return errors.New("configuration_changed")
		}
	} else if err != nil || !os.SameFile(original, current) || current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()) {
		return errors.New("configuration_changed")
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
