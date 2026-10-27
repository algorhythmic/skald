package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
	"github.com/algorhythmic/skald/sessioncapture"
)

const prefixCheckpointLimit = 4096

// This cache is disposable, not a capture checkpoint or acceptance receipt.
// SHA state can include a partial source block, so it must remain private.
type filePrefixCheckpoint struct {
	Version int                        `json:"version"`
	Device  uint64                     `json:"device"`
	Inode   uint64                     `json:"inode"`
	Size    int64                      `json:"size"`
	State   sessioncapture.PrefixState `json:"state"`
}

func prefixCheckpointName(source sessioncapture.Source) string {
	sum := sha256.Sum256([]byte(source.Key()))
	return hex.EncodeToString(sum[:]) + ".json"
}

// Open the directory through its parent so a symlink in place of the cache
// directory cannot redirect reads or writes outside that parent.
func openPrefixDirectory(dir string) (*os.Root, error) {
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) {
		return nil, errors.New("invalid_prefix_directory")
	}
	if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !prefixOwned(info) {
		return nil, errors.New("private_prefix_directory_required")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("prefix_directory_replaced")
	}
	return root, nil
}

func prefixOwned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func loadFilePrefix(root *os.Root, source sessioncapture.Source, cp sessioncapture.Checkpoint, info os.FileInfo) sessioncapture.PrefixState {
	if root == nil || cp.Version != 1 {
		return sessioncapture.PrefixState{}
	}
	f, err := root.OpenFile(prefixCheckpointName(source), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sessioncapture.PrefixState{}
	}
	defer f.Close()
	cacheInfo, err := f.Stat()
	if err != nil || !cacheInfo.Mode().IsRegular() || cacheInfo.Mode().Perm() != 0600 || !prefixOwned(cacheInfo) || cacheInfo.Size() > prefixCheckpointLimit {
		return sessioncapture.PrefixState{}
	}
	data, err := io.ReadAll(io.LimitReader(f, prefixCheckpointLimit))
	// Full-limit reads are discarded, including a file enlarged after Stat.
	// A legitimate marshaled SHA state is much smaller than this ceiling.
	if err != nil || len(data) >= prefixCheckpointLimit {
		return sessioncapture.PrefixState{}
	}
	var cached filePrefixCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cached) != nil || decoder.Decode(new(any)) != io.EOF {
		return sessioncapture.PrefixState{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || cached.Version != 1 || cached.Device != uint64(st.Dev) || cached.Inode != st.Ino || cached.Size < cp.Offset || cached.Size > info.Size() || cached.State.Version != 1 || cached.State.SourceKey != source.Key() || cached.State.SourceKey != cp.SourceKey || cached.State.Offset != cp.Offset || cached.State.Offset < 0 || cached.State.Offset > info.Size() || cached.State.PrefixDigest != cp.PrefixDigest || len(cached.State.HashState) == 0 {
		return sessioncapture.PrefixState{}
	}
	return cached.State
}

func saveFilePrefix(root *os.Root, source sessioncapture.Source, cp sessioncapture.Checkpoint, state sessioncapture.PrefixState, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || state.Version != 1 || state.SourceKey != source.Key() || state.SourceKey != cp.SourceKey || state.Offset != cp.Offset || state.Offset < 0 || state.Offset > info.Size() || state.PrefixDigest != cp.PrefixDigest || len(state.HashState) == 0 {
		return errors.New("invalid_prefix_state")
	}
	data, err := json.Marshal(filePrefixCheckpoint{Version: 1, Device: uint64(st.Dev), Inode: st.Ino, Size: info.Size(), State: state})
	if err != nil {
		return err
	}
	if len(data) > prefixCheckpointLimit {
		return errors.New("prefix_state_too_large")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".prefix-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(temporary, prefixCheckpointName(source))
}

func captureFileWithPrefixState(ctx context.Context, r archive.Registration, cp sessioncapture.Checkpoint, metrics *observability.Recorder, dir string) (sessioncapture.Batch, error) {
	if dir == "" || r.Provider == sessioncapture.Devin {
		return captureFileMeasured(ctx, r, cp, metrics)
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	defer root.Close()
	f, err := root.OpenFile(r.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	if !before.Mode().IsRegular() {
		return sessioncapture.Batch{}, errors.New("regular_source_required")
	}
	// Cache failures affect performance only. Never make capture depend on a
	// missing, corrupt or unwritable optimization file.
	cacheRoot, _ := openPrefixDirectory(dir)
	if cacheRoot != nil {
		defer cacheRoot.Close()
	}
	state := loadFilePrefix(cacheRoot, r.Source, cp, before)
	measured := &measuredInput{ReadSeeker: cancelReader{f, ctx}}
	defer func() { metrics.Add("capture_input_bytes", measured.bytes) }()
	batch, next, err := sessioncapture.ReadWithPrefixState(measured, r.Source, cp, state, sessioncapture.DefaultLimits(), time.Now())
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	after, err := root.Stat(r.Path)
	if err != nil || !os.SameFile(before, after) {
		return sessioncapture.Batch{}, errors.New("source_replaced_during_read")
	}
	if after.Size() < batch.Checkpoint.Offset {
		return sessioncapture.Batch{}, errors.New("source_changed_during_read")
	}
	if cacheRoot != nil {
		_ = saveFilePrefix(cacheRoot, r.Source, batch.Checkpoint, next, after)
	}
	return batch, nil
}
