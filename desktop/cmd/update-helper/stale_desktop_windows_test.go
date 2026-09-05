//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/installlayout"
	"reasonix/internal/repair"
)

func TestTerminateSupersededDesktopsSkipsActiveAndForeignProcesses(t *testing.T) {
	installDir := t.TempDir()
	src := t.TempDir()
	for _, name := range []string{
		"reasonix-desktop.exe",
		"reasonix-cli.exe",
		"reasonix-update-helper.exe",
		"reasonix-launcher.exe",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := activateVersionedWindowsFromStaging(&repair.UpdateTransaction{
		SchemaVersion: 1,
		ToVersion:     "v1.24.0",
		TargetKind:    "file",
		TargetPath:    filepath.Join(installDir, "reasonix-desktop.exe"),
		CreatedAt:     "2026-01-01T00:00:00Z",
	}, src); err != nil {
		t.Fatal(err)
	}
	oldDesktop, err := installlayout.ActiveDesktopPath(installDir)
	if err != nil {
		t.Fatal(err)
	}
	src2 := t.TempDir()
	for _, name := range []string{
		"reasonix-desktop.exe",
		"reasonix-cli.exe",
		"reasonix-update-helper.exe",
		"reasonix-launcher.exe",
	} {
		if err := os.WriteFile(filepath.Join(src2, name), []byte("new-"+name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := activateVersionedWindowsFromStaging(&repair.UpdateTransaction{
		SchemaVersion: 1,
		ToVersion:     "v1.24.1",
		TargetKind:    "file",
		TargetPath:    filepath.Join(installDir, "reasonix-desktop.exe"),
		CreatedAt:     "2026-01-01T00:00:01Z",
	}, src2); err != nil {
		t.Fatal(err)
	}
	activeDesktop, err := installlayout.ActiveDesktopPath(installDir)
	if err != nil {
		t.Fatal(err)
	}

	originalList := listDesktopImagesFn
	originalKill := terminatePIDFn
	t.Cleanup(func() {
		listDesktopImagesFn = originalList
		terminatePIDFn = originalKill
	})
	var killed []uint32
	listDesktopImagesFn = func() []desktopImage {
		return []desktopImage{
			{pid: 11, path: oldDesktop},
			{pid: 12, path: activeDesktop},
			{pid: 13, path: filepath.Join(t.TempDir(), "reasonix-desktop.exe")},
			{pid: 14, path: filepath.Join(installDir, "reasonix-launcher.exe")},
		}
	}
	terminatePIDFn = func(pid uint32) bool {
		killed = append(killed, pid)
		return true
	}
	if n := terminateSupersededVersionedDesktops(installDir); n != 1 {
		t.Fatalf("killed %d, want 1", n)
	}
	if len(killed) != 1 || killed[0] != 11 {
		t.Fatalf("killed pids = %v, want [11]", killed)
	}
}
