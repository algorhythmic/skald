package retrieval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRetrievalParentDeathHelper(t *testing.T) {
	path := os.Getenv("SKALD_TEST_PARENT_DEATH_EXECUTABLE")
	if path == "" {
		return
	}
	// Linux parent-death signals follow the creating OS thread. Keep that
	// thread alive until the helper process is deliberately killed by its test.
	runtime.LockOSThread()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	child, err := start(context.Background(), Config{Executable: path, SHA256: hex.EncodeToString(sum[:])}, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer child.close()
	fmt.Fprintln(os.Stdout, child.cmd.Process.Pid)
	time.Sleep(30 * time.Second)
	t.Fatal("parent-death helper was not killed")
}

func TestRetrievalChildDiesWhenParentCrashes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux parent-death signal")
	}
	path := filepath.Join(t.TempDir(), "blocked-provider")
	// A provider stuck in work cannot notice stdin EOF after a parent crash.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, executable, "-test.run=^TestRetrievalParentDeathHelper$")
	parent.Env = append(os.Environ(), "SKALD_TEST_PARENT_DEATH_EXECUTABLE="+path)
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("helper failed before starting provider", scanner.Err())
	}
	pid, err := strconv.Atoi(scanner.Text())
	if err != nil || pid <= 0 {
		t.Fatalf("invalid helper provider PID %q: %v", scanner.Text(), err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("provider exited before its parent crashed", err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		// An orphan may briefly await the host's reaper; a zombie has already
		// released every plaintext memory page and open SQLite descriptor.
		end := strings.LastIndexByte(string(stat), ')')
		if end >= 0 && strings.HasPrefix(string(stat[end+1:]), " Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("provider remained alive after its parent crashed")
}
