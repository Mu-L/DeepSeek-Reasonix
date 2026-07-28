//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/repair"
)

func installMacHandoffTestDeps(
	t *testing.T,
	tx *repair.UpdateTransaction,
	pendingPath string,
	logPath string,
	claim func(string, string, time.Duration) (*repair.UpdateTransaction, func(), error),
) {
	t.Helper()
	originalRead := readMacUpdateHandoff
	originalClaim := claimMacUpdateHandoff
	originalClear := clearMacUpdateHandoff
	originalVerify := verifyMacHandoffApp
	originalLogPath := macHandoffLogPath
	readMacUpdateHandoff = func() (*repair.UpdateTransaction, error) {
		copy := *tx
		return &copy, nil
	}
	if claim == nil {
		claim = func(string, string, time.Duration) (*repair.UpdateTransaction, func(), error) {
			copy := *tx
			return &copy, func() {}, nil
		}
	}
	claimMacUpdateHandoff = claim
	clearMacUpdateHandoff = func(*repair.UpdateTransaction) error {
		return os.Remove(pendingPath)
	}
	verifyMacHandoffApp = func(string) error { return nil }
	macHandoffLogPath = func() string { return logPath }
	t.Cleanup(func() {
		readMacUpdateHandoff = originalRead
		claimMacUpdateHandoff = originalClaim
		clearMacUpdateHandoff = originalClear
		verifyMacHandoffApp = originalVerify
		macHandoffLogPath = originalLogPath
	})
}

func TestMacUpdateHandoffWaitsForExactProcessAndRollsBackLaunchFailure(t *testing.T) {
	root := t.TempDir()
	oldApp := filepath.Join(root, "Reasonix.app")
	newApp := filepath.Join(root, "staging", "Reasonix.app")
	backupApp := oldApp + ".reasonix-update-backup"
	pending := filepath.Join(root, "pending.json")
	logPath := filepath.Join(root, "update.log")
	for _, dir := range []string{oldApp, newApp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldApp, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newApp, "marker"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx := &repair.UpdateTransaction{
		ToVersion:          "v2",
		CreatedAt:          "2026-07-28T00:00:00Z",
		TargetKind:         "app-bundle",
		TargetPath:         oldApp,
		BackupPath:         backupApp,
		HandoffAppPath:     newApp,
		HandoffStagingPath: filepath.Dir(newApp),
		HandoffOwnerPID:    99999999,
	}
	installMacHandoffTestDeps(t, tx, pending, logPath, nil)

	originalOpen := openCommand
	openCommand = func(args ...string) *exec.Cmd {
		// Force LaunchServices rejection so handoff rolls back under the lock.
		return exec.Command("/bin/sh", "-c", "exit 1")
	}
	t.Cleanup(func() { openCommand = originalOpen })

	code := runMacUpdateHandoff(macUpdateHandoffConfig{
		ToVersion: tx.ToVersion,
		CreatedAt: tx.CreatedAt,
	})
	if code == 0 {
		t.Fatal("handoff should fail when LaunchServices rejects the replacement")
	}

	marker, err := os.ReadFile(filepath.Join(oldApp, "marker"))
	if err != nil {
		t.Fatalf("read restored marker: %v", err)
	}
	if string(marker) != "old" {
		t.Fatalf("restored marker = %q, want old", marker)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending transaction was not cleared: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "PID 99999999") || !strings.Contains(logText, "rolling back") {
		t.Fatalf("handoff log lacks PID/rollback diagnostics: %s", logText)
	}
}

func TestMacUpdateHandoffHoldsMutationLockDuringSwap(t *testing.T) {
	root := t.TempDir()
	oldApp := filepath.Join(root, "Reasonix.app")
	newApp := filepath.Join(root, "staging", "Reasonix.app")
	backupApp := oldApp + ".reasonix-update-backup"
	pending := filepath.Join(root, "pending.json")
	for _, dir := range []string{oldApp, newApp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldApp, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newApp, "marker"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx := &repair.UpdateTransaction{
		ToVersion:          "v2",
		CreatedAt:          "2026-07-28T00:00:00Z",
		TargetKind:         "app-bundle",
		TargetPath:         oldApp,
		BackupPath:         backupApp,
		HandoffAppPath:     newApp,
		HandoffStagingPath: filepath.Dir(newApp),
		HandoffOwnerPID:    99999999,
	}
	installMacHandoffTestDeps(
		t,
		tx,
		pending,
		filepath.Join(root, "update.log"),
		func(string, string, time.Duration) (*repair.UpdateTransaction, func(), error) {
			unlock, err := repair.LockRepairMutationsTimeout(2*time.Second, oldApp, backupApp)
			if err != nil {
				return nil, nil, err
			}
			copy := *tx
			return &copy, unlock, nil
		},
	)

	// Hold the target lock first: handoff must wait, not race past Guard.
	holder, err := repair.LockRepairMutations(oldApp)
	if err != nil {
		t.Fatal(err)
	}

	originalOpen := openCommand
	openCommand = func(args ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "exit 0")
	}
	t.Cleanup(func() { openCommand = originalOpen })

	started := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		close(started)
		done <- runMacUpdateHandoff(macUpdateHandoffConfig{
			ToVersion: tx.ToVersion,
			CreatedAt: tx.CreatedAt,
		})
	}()
	<-started
	select {
	case code := <-done:
		holder()
		t.Fatalf("handoff completed while mutation lock held: code=%d", code)
	case <-time.After(300 * time.Millisecond):
	}

	// Bundle must still be the original while the lock is held.
	if got, err := os.ReadFile(filepath.Join(oldApp, "marker")); err != nil || string(got) != "old" {
		holder()
		t.Fatalf("bundle changed while locked: %q, %v", got, err)
	}
	holder()
	code := <-done
	if code != 0 {
		t.Fatalf("handoff after unlock: exit %d", code)
	}
	if got, err := os.ReadFile(filepath.Join(oldApp, "marker")); err != nil || string(got) != "new" {
		t.Fatalf("bundle after handoff = %q, %v", got, err)
	}
}

func TestMacUpdateHandoffReverifiesStagedBundleBeforeSwap(t *testing.T) {
	root := t.TempDir()
	oldApp := filepath.Join(root, "Reasonix.app")
	newApp := filepath.Join(root, "staging", "Reasonix.app")
	backupApp := oldApp + ".reasonix-update-backup"
	pending := filepath.Join(root, "pending.json")
	for _, dir := range []string{oldApp, newApp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldApp, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newApp, "marker"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx := &repair.UpdateTransaction{
		ToVersion:          "v2",
		CreatedAt:          "2026-07-28T00:00:00Z",
		TargetKind:         "app-bundle",
		TargetPath:         oldApp,
		BackupPath:         backupApp,
		HandoffAppPath:     newApp,
		HandoffStagingPath: filepath.Dir(newApp),
		HandoffOwnerPID:    99999999,
	}
	installMacHandoffTestDeps(t, tx, pending, filepath.Join(root, "update.log"), nil)
	verifyMacHandoffApp = func(string) error { return fmt.Errorf("signature changed") }

	code := runMacUpdateHandoff(macUpdateHandoffConfig{ToVersion: tx.ToVersion, CreatedAt: tx.CreatedAt})
	if code == 0 {
		t.Fatal("handoff accepted a staged bundle that failed re-verification")
	}
	if got, err := os.ReadFile(filepath.Join(oldApp, "marker")); err != nil || string(got) != "old" {
		t.Fatalf("installed bundle changed after verification failure: %q, %v", got, err)
	}
	if _, err := os.Stat(backupApp); !os.IsNotExist(err) {
		t.Fatalf("backup was created after verification failure: %v", err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending transaction was not cleared: %v", err)
	}
}

func TestMacUpdateHandoffParserRejectsFilesystemPaths(t *testing.T) {
	_, err := parseMacUpdateHandoffArgs([]string{
		"-to-version", "v2",
		"-created-at", "2026-07-28T00:00:00Z",
		"-old-app", "/tmp/Unrelated.app",
	})
	if err == nil {
		t.Fatal("legacy filesystem path argument was accepted")
	}
}

func TestMacUpdateHandoffSerializesWithConcurrentRollback(t *testing.T) {
	// Ensure LockRepairMutations used by handoff and repair share identity.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldApp := filepath.Join(root, "Reasonix.app")
	if err := os.MkdirAll(oldApp, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	order := make(chan string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		unlock, err := repair.LockRepairMutationsTimeout(2*time.Second, oldApp)
		if err != nil {
			t.Errorf("first lock: %v", err)
			return
		}
		order <- "a"
		time.Sleep(200 * time.Millisecond)
		unlock()
	}()
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		unlock, err := repair.LockRepairMutationsTimeout(2*time.Second, oldApp)
		if err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		order <- "b"
		unlock()
	}()
	wg.Wait()
	first := <-order
	second := <-order
	if first != "a" || second != "b" {
		t.Fatalf("lock order = %s then %s, want a then b", first, second)
	}
}
