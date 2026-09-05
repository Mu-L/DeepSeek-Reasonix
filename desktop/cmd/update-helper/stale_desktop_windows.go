//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"reasonix/internal/installlayout"
)

var (
	listDesktopPIDsFn       = listDesktopPIDs
	openDesktopProcessFn    = windows.OpenProcess
	queryDesktopImagePathFn = processImagePath
	terminateProcessFn      = windows.TerminateProcess
	waitDesktopProcessFn    = windows.WaitForSingleObject
	closeDesktopProcessFn   = windows.CloseHandle
)

const supersededDesktopExitTimeout = 10 * time.Second

func terminateSupersededVersionedDesktops(installRoot string) (int, error) {
	if !installlayout.HasCurrent(installRoot) {
		return 0, nil
	}
	self := uint32(os.Getpid())
	killed := 0
	var errs []error
	for _, pid := range listDesktopPIDsFn() {
		if pid == 0 || pid == self {
			continue
		}
		terminated, err := terminateSupersededDesktopPID(installRoot, pid, supersededDesktopExitTimeout)
		if err != nil {
			errs = append(errs, err)
		} else if terminated {
			killed++
		}
	}
	return killed, errors.Join(errs...)
}

func listDesktopPIDs() []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	out := make([]uint32, 0, 8)
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if name != "reasonix-desktop.exe" {
			continue
		}
		out = append(out, pe.ProcessID)
	}
	return out
}

func processImagePath(h windows.Handle) (string, error) {
	size := uint32(32768)
	buf := make([]uint16, size)
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

func terminateSupersededDesktopPID(installRoot string, pid uint32, timeout time.Duration) (bool, error) {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_TERMINATE | windows.SYNCHRONIZE)
	h, err := openDesktopProcessFn(access, false, pid)
	if err != nil {
		return false, nil
	}
	defer func() { _ = closeDesktopProcessFn(h) }()
	path, err := queryDesktopImagePathFn(h)
	if err != nil || !installlayout.IsSupersededVersionedDesktop(installRoot, path) {
		return false, nil
	}
	if err := terminateProcessFn(h, 1); err != nil {
		return false, fmt.Errorf("terminate superseded desktop PID %d: %w", pid, err)
	}
	waitMS := uint32(timeout / time.Millisecond)
	result, err := waitDesktopProcessFn(h, waitMS)
	if err != nil {
		return false, fmt.Errorf("wait for superseded desktop PID %d: %w", pid, err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return false, fmt.Errorf("wait for superseded desktop PID %d: result %d", pid, result)
	}
	return true, nil
}
