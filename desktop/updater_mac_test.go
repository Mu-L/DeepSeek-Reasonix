//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/repair"
)

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

	originalOpen := openCommand
	openCommand = func(args ...string) *exec.Cmd {
		// Force LaunchServices rejection so handoff rolls back under the lock.
		return exec.Command("/bin/sh", "-c", "exit 1")
	}
	t.Cleanup(func() { openCommand = originalOpen })

	code := runMacUpdateHandoff(macUpdateHandoffConfig{
		OldApp:        oldApp,
		NewApp:        newApp,
		BackupApp:     backupApp,
		PendingUpdate: pending,
		Staging:       filepath.Dir(newApp),
		LogPath:       logPath,
		// Non-existent PID: wait returns immediately.
		OldPID: 99999999,
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
			OldApp:        oldApp,
			NewApp:        newApp,
			BackupApp:     backupApp,
			PendingUpdate: pending,
			Staging:       filepath.Dir(newApp),
			LogPath:       filepath.Join(root, "update.log"),
			OldPID:        99999999,
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
