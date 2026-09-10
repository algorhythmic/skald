package daemon

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

type fileStamp struct {
	Device, Inode     uint64
	Size              int64
	Modified, Changed syscall.Timespec
}
type verifiedFile struct {
	Stamp      fileStamp
	Checkpoint sessioncapture.Checkpoint
	At         time.Time
}

func sourceStamp(source archive.Registration) (fileStamp, error) {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return fileStamp{}, err
	}
	defer root.Close()
	info, err := root.Stat(source.Path)
	if err != nil {
		return fileStamp{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return fileStamp{}, errors.New("regular_source_required")
	}
	return fileStamp{Device: uint64(st.Dev), Inode: st.Ino, Size: info.Size(), Modified: st.Mtim, Changed: st.Ctim}, nil
}
