//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

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

	originalList := listDesktopPIDsFn
	originalOpen := openDesktopProcessFn
	originalQuery := queryDesktopImagePathFn
	originalTerminate := terminateProcessFn
	originalWait := waitDesktopProcessFn
	originalClose := closeDesktopProcessFn
	t.Cleanup(func() {
		listDesktopPIDsFn = originalList
		openDesktopProcessFn = originalOpen
		queryDesktopImagePathFn = originalQuery
		terminateProcessFn = originalTerminate
		waitDesktopProcessFn = originalWait
		closeDesktopProcessFn = originalClose
	})
	listDesktopPIDsFn = func() []uint32 { return []uint32{11, 12, 13, 14} }
	openDesktopProcessFn = func(_ uint32, _ bool, pid uint32) (windows.Handle, error) {
		return windows.Handle(pid), nil
	}
	paths := map[windows.Handle]string{
		11: oldDesktop,
		12: activeDesktop,
		13: filepath.Join(t.TempDir(), "reasonix-desktop.exe"),
		14: filepath.Join(installDir, "reasonix-launcher.exe"),
	}
	queryDesktopImagePathFn = func(h windows.Handle) (string, error) { return paths[h], nil }
	var terminated, waited []windows.Handle
	terminateProcessFn = func(h windows.Handle, _ uint32) error {
		terminated = append(terminated, h)
		return nil
	}
	waitDesktopProcessFn = func(h windows.Handle, _ uint32) (uint32, error) {
		waited = append(waited, h)
		return windows.WAIT_OBJECT_0, nil
	}
	closeDesktopProcessFn = func(windows.Handle) error { return nil }
	n, err := terminateSupersededVersionedDesktops(installDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("killed %d, want 1", n)
	}
	if len(terminated) != 1 || terminated[0] != 11 {
		t.Fatalf("terminated handles = %v, want [11]", terminated)
	}
	if len(waited) != 1 || waited[0] != terminated[0] {
		t.Fatalf("waited handles = %v, want terminated handle %v", waited, terminated)
	}
}

func TestTerminateSupersededDesktopPIDRequiresConfirmedExit(t *testing.T) {
	root := t.TempDir()
	oldDesktop := seedSupersededDesktopLayout(t, root)
	originalOpen := openDesktopProcessFn
	originalQuery := queryDesktopImagePathFn
	originalTerminate := terminateProcessFn
	originalWait := waitDesktopProcessFn
	originalClose := closeDesktopProcessFn
	t.Cleanup(func() {
		openDesktopProcessFn = originalOpen
		queryDesktopImagePathFn = originalQuery
		terminateProcessFn = originalTerminate
		waitDesktopProcessFn = originalWait
		closeDesktopProcessFn = originalClose
	})
	const handle = windows.Handle(77)
	openDesktopProcessFn = func(uint32, bool, uint32) (windows.Handle, error) { return handle, nil }
	queryDesktopImagePathFn = func(got windows.Handle) (string, error) {
		if got != handle {
			t.Fatalf("query handle = %v, want %v", got, handle)
		}
		return oldDesktop, nil
	}
	terminateProcessFn = func(got windows.Handle, _ uint32) error {
		if got != handle {
			t.Fatalf("terminate handle = %v, want %v", got, handle)
		}
		return nil
	}
	waitDesktopProcessFn = func(got windows.Handle, _ uint32) (uint32, error) {
		if got != handle {
			t.Fatalf("wait handle = %v, want %v", got, handle)
		}
		return uint32(windows.WAIT_TIMEOUT), nil
	}
	closeDesktopProcessFn = func(windows.Handle) error { return nil }
	terminated, err := terminateSupersededDesktopPID(root, 77, time.Millisecond)
	if err == nil || terminated {
		t.Fatalf("terminated=%v err=%v, want a confirmed-exit failure", terminated, err)
	}
}

func seedSupersededDesktopLayout(t *testing.T, root string) string {
	t.Helper()
	activate := func(version, prefix string) string {
		t.Helper()
		src := t.TempDir()
		for _, name := range []string{
			"reasonix-desktop.exe",
			"reasonix-cli.exe",
			"reasonix-update-helper.exe",
			"reasonix-launcher.exe",
		} {
			if err := os.WriteFile(filepath.Join(src, name), []byte(prefix+name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := activateVersionedWindowsFromStaging(&repair.UpdateTransaction{
			SchemaVersion: 1,
			ToVersion:     version,
			TargetKind:    "file",
			TargetPath:    filepath.Join(root, "reasonix-desktop.exe"),
			CreatedAt:     "2026-01-01T00:00:00Z",
		}, src); err != nil {
			t.Fatal(err)
		}
		desktop, err := installlayout.ActiveDesktopPath(root)
		if err != nil {
			t.Fatal(err)
		}
		return desktop
	}
	oldDesktop := activate("v1.24.0", "old-")
	_ = activate("v1.24.1", "new-")
	return oldDesktop
}
