# Reasonix systray patches

This directory is copied from `fyne.io/systray` version
`v1.12.3-0.20260814134402-f60f01be81c6`.

Reasonix adds `SetIconID` on Windows and registers the notification icon with
`NIF_GUID`. Release builds pass a fixed GUID before `Run`, which lets Windows
keep the user's notification-area visibility preference when the signed desktop
executable moves between `versions/<version>/` directories.

When updating the upstream copy, preserve the `SetIconID` API and ensure the
configured GUID is included in every `Shell_NotifyIcon` add, modify, delete, and
Explorer restart operation.
