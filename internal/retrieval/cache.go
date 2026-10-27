package retrieval

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

var publicationName = regexp.MustCompile(`^publication-[0-9]+$`)

// leaseCache holds an exclusive directory-inode lease until Close. A second
// daemon cannot purge or query another daemon's plaintext publications.
func leaseCache(path string) (*os.File, *os.Root, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, *os.Root, error) { file.Close(); return nil, nil, err }
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fail(errors.New("retrieval_cache_directory_not_private"))
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(errors.New("retrieval_cache_in_use"))
	}
	root, err := os.OpenRoot("/proc/self/fd/" + itoaFD(fd))
	if err != nil {
		return fail(err)
	}
	return file, root, nil
}
func itoaFD(fd int) string { return strconv.Itoa(fd) }

// purgePublication validates the entire flat publication before unlinking any
// entry. Root operations never traverse a symlink into another directory.
func (e *Engine) purgePublication(name string) error {
	if !publicationName.MatchString(name) {
		return errors.New("invalid_retrieval_publication_name")
	}
	info, err := e.cacheRoot.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe_retrieval_publication")
	}
	child, err := e.cacheRoot.OpenRoot(name)
	if err != nil {
		return err
	}
	defer child.Close()
	dir, err := child.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := child.Lstat(entry.Name())
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		allowed := entry.Name() == "braid.yaml" || entry.Name() == "index.sqlite" || entry.Name() == "index.sqlite-wal" || entry.Name() == "index.sqlite-shm" || entry.Name() == "index.sqlite-journal"
		if !allowed || !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe_retrieval_publication_entry")
		}
	}
	for _, entry := range entries {
		if err := child.Remove(entry.Name()); err != nil {
			return err
		}
	}
	return e.cacheRoot.Remove(name)
}
func (e *Engine) purgeOrphans() error {
	dir, err := e.cacheRoot.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !publicationName.MatchString(entry.Name()) {
			return errors.New("unexpected_retrieval_cache_entry")
		}
		if err := e.purgePublication(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) cacheIdentity() error {
	if e.lease == nil {
		return errors.New("retrieval_engine_closed")
	}
	held, err := e.lease.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(e.config.Directory)
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) || !current.IsDir() || current.Mode().Perm()&0077 != 0 {
		return errors.New("retrieval_cache_directory_changed")
	}
	return nil
}

// WithInvalidation serializes query publication, cache withdrawal, and an
// archive mutation. Cleanup must succeed before the callback can write.
func (e *Engine) WithInvalidation(mutate func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.cacheIdentity(); err != nil {
		return err
	}
	if err := e.invalidate(); err != nil {
		return err
	}
	if mutate == nil {
		return nil
	}
	return mutate()
}
func (e *Engine) invalidate() error {
	var errs []error
	for key := range e.entries {
		if err := e.remove(key); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := e.purgeOrphans(); err != nil {
			errs = append(errs, err)
		}
	}
	e.cleanupErr = errors.Join(errs...)
	return e.cleanupErr
}
func (e *Engine) remove(key string) error {
	p := e.entries[key]
	if p == nil {
		return nil
	}
	if p.client != nil {
		p.client.close()
	}
	if err := e.purgePublication(filepath.Base(p.directory)); err != nil {
		e.cleanupErr = err
		return err
	}
	delete(e.entries, key)
	return nil
}
