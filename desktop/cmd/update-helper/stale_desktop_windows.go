//go:build windows

package main

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"reasonix/internal/installlayout"
)

type desktopImage struct {
	pid  uint32
	path string
}

var (
	listDesktopImagesFn = listDesktopImages
	terminatePIDFn      = terminatePID
)

func terminateSupersededVersionedDesktops(installRoot string) int {
	if !installlayout.HasCurrent(installRoot) {
		return 0
	}
	self := uint32(os.Getpid())
	killed := 0
	for _, proc := range listDesktopImagesFn() {
		if proc.pid == 0 || proc.pid == self {
			continue
		}
		if !installlayout.IsSupersededVersionedDesktop(installRoot, proc.path) {
			continue
		}
		if terminatePIDFn(proc.pid) {
			killed++
		}
	}
	return killed
}

func listDesktopImages() []desktopImage {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	out := make([]desktopImage, 0, 8)
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if name != "reasonix-desktop.exe" {
			continue
		}
		path, err := processImagePath(pe.ProcessID)
		if err != nil || path == "" {
			continue
		}
		out = append(out, desktopImage{pid: pe.ProcessID, path: path})
	}
	return out
}

func processImagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	size := uint32(32768)
	buf := make([]uint16, size)
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

func terminatePID(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 1) == nil
}
