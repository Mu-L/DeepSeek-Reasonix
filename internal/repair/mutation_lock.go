package repair

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/filelock"
)

const repairMutationLockTimeout = 5 * time.Second

// repairMutationBeforeLock is a test seam for forcing competing repair
// operations to overlap before one waits on the shared file lock.
var repairMutationBeforeLock = func([]string) {}

// repairMutationBeforeRename is a test seam for changing a target in the
// narrow interval between its final state check and quarantine rename.
var repairMutationBeforeRename = func(string) {}

// repairMutationAfterRename is a test seam for forcing an uncooperative writer
// to create a new target after the confirmed node has been quarantined.
var repairMutationAfterRename = func(string) {}

func lockRepairTransaction() (func(), error) {
	unlock, err := lockRepairMutations(repairTransactionPath())
	if err != nil {
		return nil, fmt.Errorf("lock repair transaction: %w", err)
	}
	return unlock, nil
}

func restoreRepairNodeIfAbsent(backup, target string) error {
	info, err := os.Lstat(backup)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(backup)
		if err != nil {
			return err
		}
		if err := os.Symlink(linkTarget, target); err != nil {
			return err
		}
		return os.Remove(backup)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("restore repair target: unsupported backup type %s", info.Mode().Type())
	}
	if err := os.Link(backup, target); err != nil {
		return err
	}
	return os.Remove(backup)
}

// lockRepairMutations serializes repair read-check-write cycles by canonical
// target path. Lock files live in Reasonix state rather than beside project or
// configuration files, and paths are sorted so multi-target actions cannot
// deadlock each other.
func lockRepairMutations(paths ...string) (func(), error) {
	lockDir := config.RepairMutationLockDir()
	if lockDir == "" {
		return nil, fmt.Errorf("lock repair mutations: OS user cache directory is unavailable")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("lock repair mutations: create lock directory: %w", err)
	}

	unique := map[string]struct{}{}
	keys := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("lock repair mutations: resolve target: %w", err)
		}
		key := filepath.Clean(absolute)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(filepath.ToSlash(key))
		}
		if _, ok := unique[key]; ok {
			continue
		}
		unique[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return func() {}, nil
	}
	sort.Strings(keys)
	repairMutationBeforeLock(append([]string(nil), keys...))

	ctx, cancel := context.WithTimeout(context.Background(), repairMutationLockTimeout)
	defer cancel()
	releases := make([]func(), 0, len(keys))
	for _, key := range keys {
		digest := sha256.Sum256([]byte(key))
		lockPath := filepath.Join(lockDir, fmt.Sprintf("%x.lock", digest))
		release, err := filelock.Acquire(ctx, lockPath)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, fmt.Errorf("lock repair mutations: %w", err)
		}
		releases = append(releases, release)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}, nil
}
