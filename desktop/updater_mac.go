//go:build darwin

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/repair"
)

const (
	macBundleID         = "com.wails.reasonix-desktop"
	macUpdateHandoffArg = "--reasonix-mac-update-handoff"
	// macUpdateHandoffLockTimeout covers concurrent Guard rollback/prepare while
	// the critical directory swap runs. PID wait happens before the lock so a
	// long exit wait does not starve unrelated repairs.
	macUpdateHandoffLockTimeout = 2 * time.Minute
)

// openCommand is a test seam so handoff tests can inject a failing open(1).
var openCommand = func(args ...string) *exec.Cmd {
	return exec.Command("open", args...)
}

func applyMac(zipPath, targetVersion string) error {
	if !macSelfUpdateAllowed() {
		return fmt.Errorf("macOS automatic update is not enabled for this build")
	}
	currentApp, err := currentMacAppBundle()
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "reasonix-mac-update-*")
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := exec.Command("ditto", "-x", "-k", zipPath, staging).Run(); err != nil {
		return fmt.Errorf("extract macOS update: %w", err)
	}
	nextApp, err := findMacApp(staging)
	if err != nil {
		return err
	}
	if err := verifyMacApp(nextApp); err != nil {
		return err
	}
	backupApp := currentApp + ".reasonix-update-backup"
	if _, err := repair.PrepareAppBundleUpdate(version, targetVersion, currentApp, backupApp); err != nil {
		return err
	}
	cacheDir, err := updateCacheDir()
	if err != nil {
		_ = repair.CancelPendingUpdate(targetVersion)
		return err
	}
	logPath := filepath.Join(cacheDir, "update-helper.log")
	exe, err := os.Executable()
	if err != nil {
		_ = repair.CancelPendingUpdate(targetVersion)
		return err
	}
	// Detach a self-subprocess that holds the shared repair mutation lock for
	// the actual mv/ditto window. A shell helper cannot share Go flock keys, so
	// the binary that performs the directory swap must take LockRepairMutations.
	cmd := exec.Command(exe,
		macUpdateHandoffArg,
		"-old-app", currentApp,
		"-new-app", nextApp,
		"-backup-app", backupApp,
		"-pending-update", repair.PendingUpdatePath(),
		"-staging", staging,
		"-log", logPath,
		"-old-pid", strconv.Itoa(os.Getpid()),
	)
	if err := cmd.Start(); err != nil {
		_ = repair.CancelPendingUpdate(targetVersion)
		return err
	}
	handedOff = true
	return nil
}

// maybeRunMacUpdateHandoff handles the detached self-update child before Wails
// or single-instance setup runs.
func maybeRunMacUpdateHandoff(args []string) (handled bool, exitCode int) {
	if len(args) == 0 || args[0] != macUpdateHandoffArg {
		return false, 0
	}
	cfg, err := parseMacUpdateHandoffArgs(args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "macOS update handoff:", err)
		return true, 2
	}
	return true, runMacUpdateHandoff(cfg)
}

type macUpdateHandoffConfig struct {
	OldApp        string
	NewApp        string
	BackupApp     string
	PendingUpdate string
	Staging       string
	LogPath       string
	OldPID        int
}

func parseMacUpdateHandoffArgs(args []string) (macUpdateHandoffConfig, error) {
	fs := flag.NewFlagSet("reasonix-mac-update-handoff", flag.ContinueOnError)
	var cfg macUpdateHandoffConfig
	fs.StringVar(&cfg.OldApp, "old-app", "", "current app bundle path")
	fs.StringVar(&cfg.NewApp, "new-app", "", "replacement app bundle path")
	fs.StringVar(&cfg.BackupApp, "backup-app", "", "backup app bundle path")
	fs.StringVar(&cfg.PendingUpdate, "pending-update", "", "pending update transaction path")
	fs.StringVar(&cfg.Staging, "staging", "", "staging directory to remove on success")
	fs.StringVar(&cfg.LogPath, "log", "", "handoff log path")
	fs.IntVar(&cfg.OldPID, "old-pid", 0, "desktop PID to wait for")
	if err := fs.Parse(args); err != nil {
		return macUpdateHandoffConfig{}, err
	}
	if cfg.OldApp == "" || cfg.NewApp == "" || cfg.BackupApp == "" || cfg.PendingUpdate == "" || cfg.OldPID <= 0 {
		return macUpdateHandoffConfig{}, fmt.Errorf("missing required handoff arguments")
	}
	return cfg, nil
}

func runMacUpdateHandoff(cfg macUpdateHandoffConfig) int {
	logFile := appendMacHandoffLog(cfg.LogPath)
	logf := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintln(os.Stderr, msg)
		if logFile != nil {
			fmt.Fprintln(logFile, msg)
		}
	}
	if logFile != nil {
		defer logFile.Close()
	}
	logf("macOS update handoff started for PID %d", cfg.OldPID)

	// Wait for the exact desktop PID before taking mutation locks so a long
	// exit wait does not block unrelated project repairs.
	if err := waitForPIDExit(cfg.OldPID, 60*time.Second); err != nil {
		logf("timed out waiting for PID %d to exit: %v", cfg.OldPID, err)
		_ = os.Remove(cfg.PendingUpdate)
		_ = os.RemoveAll(cfg.Staging)
		_ = openCommand(cfg.OldApp).Start()
		return 1
	}

	// Hold the same target locks as Guard rollback for the directory swap so
	// concurrent restore cannot interleave with ditto and produce a mixed bundle.
	unlock, err := repair.LockRepairMutationsTimeout(macUpdateHandoffLockTimeout, cfg.OldApp, cfg.BackupApp)
	if err != nil {
		logf("failed to acquire mutation lock: %v", err)
		_ = os.Remove(cfg.PendingUpdate)
		_ = os.RemoveAll(cfg.Staging)
		_ = openCommand(cfg.OldApp).Start()
		return 1
	}
	defer unlock()

	// A concurrent Guard rollback may have already consumed the pending
	// transaction while we waited for the desktop to exit.
	if _, err := os.Stat(cfg.PendingUpdate); err != nil {
		logf("pending update missing after wait; aborting handoff: %v", err)
		_ = os.RemoveAll(cfg.Staging)
		return 1
	}

	rollback := func() {
		logf("rolling back macOS update")
		_ = os.RemoveAll(cfg.OldApp)
		if err := os.Rename(cfg.BackupApp, cfg.OldApp); err != nil {
			logf("failed to restore backup bundle: %v", err)
		}
		_ = os.Remove(cfg.PendingUpdate)
		_ = exec.Command("xattr", "-dr", "com.apple.quarantine", cfg.OldApp).Run()
		if err := openCommand("-n", cfg.OldApp).Run(); err != nil {
			_ = openCommand(cfg.OldApp).Run()
		}
		_ = os.RemoveAll(cfg.Staging)
	}

	_ = os.RemoveAll(cfg.BackupApp)
	if err := os.Rename(cfg.OldApp, cfg.BackupApp); err != nil {
		logf("failed to move current app bundle to backup: %v", err)
		_ = os.Remove(cfg.PendingUpdate)
		_ = os.RemoveAll(cfg.Staging)
		_ = openCommand(cfg.OldApp).Start()
		return 1
	}
	if err := exec.Command("ditto", cfg.NewApp, cfg.OldApp).Run(); err != nil {
		logf("failed to copy replacement app bundle: %v", err)
		rollback()
		return 1
	}
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", cfg.OldApp).Run()
	if err := openCommand("-n", cfg.OldApp).Run(); err != nil {
		logf("LaunchServices rejected the replacement app bundle: %v", err)
		rollback()
		return 1
	}
	logf("replacement app bundle launched")
	_ = os.RemoveAll(cfg.Staging)
	return 0
}

func appendMacHandoffLog(path string) *os.File {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func waitForPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !macProcessAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process still running")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func macProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence without delivering a real signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func currentMacAppBundle() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	const marker = ".app/Contents/MacOS/"
	idx := strings.Index(exe, marker)
	if idx < 0 {
		return "", fmt.Errorf("update: current executable is not inside a macOS .app bundle")
	}
	app := exe[:idx+len(".app")]
	if _, err := os.Stat(filepath.Join(app, "Contents", "Info.plist")); err != nil {
		return "", fmt.Errorf("update: current app bundle is invalid: %w", err)
	}
	return app, nil
}

func findMacApp(root string) (string, error) {
	direct := filepath.Join(root, "Reasonix.app")
	if _, err := os.Stat(filepath.Join(direct, "Contents", "Info.plist")); err == nil {
		return direct, nil
	}
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return err
		}
		if d.IsDir() && strings.HasSuffix(path, ".app") {
			if _, statErr := os.Stat(filepath.Join(path, "Contents", "Info.plist")); statErr == nil {
				found = path
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("update: no .app bundle found in macOS update archive")
	}
	return found, nil
}

func verifyMacApp(appPath string) error {
	info := filepath.Join(appPath, "Contents", "Info.plist")
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :CFBundleIdentifier", info).Output()
	if err != nil {
		return fmt.Errorf("read macOS bundle identifier: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != macBundleID {
		return fmt.Errorf("update: bundle identifier %q does not match %q", got, macBundleID)
	}
	if err := exec.Command("codesign", "--verify", "--deep", "--strict", appPath).Run(); err != nil {
		return fmt.Errorf("verify macOS code signature: %w", err)
	}
	if err := exec.Command("spctl", "--assess", "--type", "execute", appPath).Run(); err != nil {
		return fmt.Errorf("assess macOS notarization: %w", err)
	}
	return nil
}
