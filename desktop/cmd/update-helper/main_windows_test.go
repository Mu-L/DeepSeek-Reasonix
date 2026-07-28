//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/repair"
)

func TestRunRequiresTargetVersionBeforeStartingInstaller(t *testing.T) {
	if code := run([]string{"--installer", `C:\Temp\Reasonix-installer.exe`}); code != 2 {
		t.Fatalf("run without --to-version = %d, want 2", code)
	}
}

func TestRunHoldsReleaseUnitLockAcrossInstallerHandoff(t *testing.T) {
	installDir := t.TempDir()
	var events []string
	originalWait := waitForProcessExitFn
	originalInstaller := runInstallerFn
	originalRelaunch := startRelaunchFn
	originalClaim := claimPendingFileUpdateFn
	t.Cleanup(func() {
		waitForProcessExitFn = originalWait
		runInstallerFn = originalInstaller
		startRelaunchFn = originalRelaunch
		claimPendingFileUpdateFn = originalClaim
	})
	waitForProcessExitFn = func(uint32, time.Duration) error {
		events = append(events, "wait")
		return nil
	}
	runInstallerFn = func(string, string) error {
		events = append(events, "installer")
		return nil
	}
	startRelaunchFn = func(string, string) error {
		events = append(events, "relaunch")
		return nil
	}
	claimPendingFileUpdateFn = func(toVersion, createdAt, launcherPath string, paths []string, _ time.Duration) (*repair.UpdateTransaction, func(), error) {
		events = append(events, "claim:"+strings.Join([]string{toVersion, createdAt, launcherPath, strings.Join(paths, "\x00")}, "\x01"))
		return &repair.UpdateTransaction{}, func() { events = append(events, "release") }, nil
	}

	if code := run([]string{
		"--parent-pid", "1234",
		"--installer", filepath.Join(installDir, "installer.exe"),
		"--install-dir", installDir,
		"--relaunch", filepath.Join(installDir, "reasonix-desktop.exe"),
		"--to-version", "v2",
		"--created-at", "2026-07-29T00:00:00Z",
	}); code != 0 {
		t.Fatalf("run exit code = %d", code)
	}
	wantClaim := "claim:" + strings.Join([]string{
		"v2",
		"2026-07-29T00:00:00Z",
		filepath.Join(installDir, "reasonix-desktop.exe"),
		strings.Join(windowsReleaseUnitPaths(installDir), "\x00"),
	}, "\x01")
	if len(events) != 5 || events[0] != wantClaim ||
		events[1] != "wait" || events[2] != "installer" || events[3] != "relaunch" || events[4] != "release" {
		t.Fatalf("handoff events = %#v", events)
	}
}
