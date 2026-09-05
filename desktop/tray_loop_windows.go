//go:build windows

package main

import (
	"log/slog"
	"os"
	"runtime"

	"fyne.io/systray"
)

const windowsTrayIconID = "{AF8B2B6E-CF17-43B9-AFB9-B0BF2695D8AC}"

func startDesktopTray(onReady, onExit func()) func() {
	if os.Getenv("REASONIX_DEV") == "" {
		if err := systray.SetIconID(windowsTrayIconID); err != nil {
			slog.Warn("desktop: configure stable tray identity", "err", err)
		}
	}
	go runDesktopTrayLoop(func() {
		systray.Run(onReady, onExit)
	})
	return systray.Quit
}

func runDesktopTrayLoop(run func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	run()
}
