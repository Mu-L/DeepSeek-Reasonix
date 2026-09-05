//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsTrayIconIDIsStableAndValid(t *testing.T) {
	if _, err := windows.GUIDFromString(windowsTrayIconID); err != nil {
		t.Fatalf("tray icon ID: %v", err)
	}
}
