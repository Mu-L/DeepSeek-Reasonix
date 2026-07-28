//go:build darwin

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

var (
	// Test seams keep desktop tests independent of a real signed bundle and the
	// process executable path used by repair transaction validation.
	openCommand = func(args ...string) *exec.Cmd {
		return exec.Command("open", args...)
	}
	readMacUpdateHandoff  = repair.ReadPendingUpdate
	claimMacUpdateHandoff = repair.ClaimPendingAppBundleUpdateHandoff
	clearMacUpdateHandoff = repair.ClearClaimedAppBundleUpdateHandoff
	verifyMacHandoffApp   = verifyMacApp
	macHandoffRename      = os.Rename
	macHandoffLogPath     = func() string {
		cacheDir, err := updateCacheDir()
		if err != nil {
			return ""
		}
		return filepath.Join(cacheDir, "update-helper.log")
	}
)

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
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	tx, err := repair.PrepareAppBundleUpdateHandoff(
		version,
		targetVersion,
		currentApp,
		backupApp,
		nextApp,
		staging,
		os.Getpid(),
	)
	if err != nil {
		return err
	}
	// Detach a self-subprocess that holds the shared repair mutation lock for
	// the actual mv/ditto window. A shell helper cannot share Go flock keys, so
	// the binary that performs the directory swap must take LockRepairMutations.
	cmd := exec.Command(exe,
		macUpdateHandoffArg,
		"-to-version", tx.ToVersion,
		"-created-at", tx.CreatedAt,
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
	ToVersion string
	CreatedAt string
}

func parseMacUpdateHandoffArgs(args []string) (macUpdateHandoffConfig, error) {
	fs := flag.NewFlagSet("reasonix-mac-update-handoff", flag.ContinueOnError)
	var cfg macUpdateHandoffConfig
	fs.StringVar(&cfg.ToVersion, "to-version", "", "pending update target version")
	fs.StringVar(&cfg.CreatedAt, "created-at", "", "pending update creation timestamp")
	if err := fs.Parse(args); err != nil {
		return macUpdateHandoffConfig{}, err
	}
	if fs.NArg() != 0 {
		return macUpdateHandoffConfig{}, fmt.Errorf("unexpected handoff arguments")
	}
	cfg.ToVersion = strings.TrimSpace(cfg.ToVersion)
	cfg.CreatedAt = strings.TrimSpace(cfg.CreatedAt)
	if cfg.ToVersion == "" || cfg.CreatedAt == "" {
		return macUpdateHandoffConfig{}, fmt.Errorf("missing required handoff arguments")
	}
	return cfg, nil
}

func runMacUpdateHandoff(cfg macUpdateHandoffConfig) int {
	logFile := appendMacHandoffLog(macHandoffLogPath())
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
	pending, err := readMacUpdateHandoff()
	if err != nil {
		logf("cannot read pending update handoff: %v", err)
		return 1
	}
	if strings.TrimSpace(pending.ToVersion) != cfg.ToVersion ||
		strings.TrimSpace(pending.CreatedAt) != cfg.CreatedAt ||
		pending.HandoffOwnerPID <= 0 {
		logf("pending update does not match handoff identity")
		return 1
	}
	logf("macOS update handoff started for PID %d", pending.HandoffOwnerPID)

	// Wait for the exact desktop PID before taking mutation locks so a long
	// exit wait does not block unrelated project repairs.
	if err := waitForPIDExit(pending.HandoffOwnerPID, 60*time.Second); err != nil {
		logf("timed out waiting for PID %d to exit: %v", pending.HandoffOwnerPID, err)
		if claimed, release, claimErr := claimMacUpdateHandoff(cfg.ToVersion, cfg.CreatedAt, macUpdateHandoffLockTimeout); claimErr == nil {
			if clearErr := clearMacUpdateHandoff(claimed); clearErr != nil {
				logf("failed to clear timed-out handoff: %v", clearErr)
			}
			_ = os.RemoveAll(claimed.HandoffStagingPath)
			release()
		}
		return 1
	}

	// Claim re-reads the full pending transaction while holding both its state
	// lock and the same target locks as Guard rollback.
	claimed, release, err := claimMacUpdateHandoff(cfg.ToVersion, cfg.CreatedAt, macUpdateHandoffLockTimeout)
	if err != nil {
		logf("failed to claim pending update handoff: %v", err)
		return 1
	}
	defer release()
	oldApp := claimed.TargetPath
	newApp := claimed.HandoffAppPath
	backupApp := claimed.BackupPath
	staging := claimed.HandoffStagingPath
	clearPending := func() {
		if err := clearMacUpdateHandoff(claimed); err != nil {
			logf("failed to clear pending update handoff: %v", err)
		}
	}
	if err := verifyMacHandoffApp(newApp); err != nil {
		logf("replacement app bundle no longer verifies: %v", err)
		clearPending()
		_ = os.RemoveAll(staging)
		_ = openCommand(oldApp).Start()
		return 1
	}

	rollback := func() error {
		logf("rolling back macOS update")
		failedApp := oldApp + ".reasonix-update-failed"
		if err := os.RemoveAll(failedApp); err != nil {
			return fmt.Errorf("remove prior failed replacement bundle: %w", err)
		}
		retainedFailedApp := true
		if err := macHandoffRename(oldApp, failedApp); err != nil {
			if os.IsNotExist(err) {
				retainedFailedApp = false
			} else {
				return fmt.Errorf("retain failed replacement bundle: %w", err)
			}
		}
		if err := macHandoffRename(backupApp, oldApp); err != nil {
			if retainedFailedApp {
				if compensateErr := macHandoffRename(failedApp, oldApp); compensateErr != nil {
					return fmt.Errorf("restore backup bundle: %w (failed to restore replacement bundle: %v)", err, compensateErr)
				}
			}
			return fmt.Errorf("restore backup bundle: %w", err)
		}
		_ = os.RemoveAll(failedApp)
		clearPending()
		_ = exec.Command("xattr", "-dr", "com.apple.quarantine", oldApp).Run()
		if err := openCommand("-n", oldApp).Run(); err != nil {
			_ = openCommand(oldApp).Run()
		}
		_ = os.RemoveAll(staging)
		return nil
	}

	_ = os.RemoveAll(backupApp)
	if err := macHandoffRename(oldApp, backupApp); err != nil {
		logf("failed to move current app bundle to backup: %v", err)
		clearPending()
		_ = os.RemoveAll(staging)
		_ = openCommand(oldApp).Start()
		return 1
	}
	if err := exec.Command("ditto", newApp, oldApp).Run(); err != nil {
		logf("failed to copy replacement app bundle: %v", err)
		if rollbackErr := rollback(); rollbackErr != nil {
			logf("failed to restore backup bundle: %v", rollbackErr)
		}
		return 1
	}
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", oldApp).Run()
	if err := openCommand("-n", oldApp).Run(); err != nil {
		logf("LaunchServices rejected the replacement app bundle: %v", err)
		if rollbackErr := rollback(); rollbackErr != nil {
			logf("failed to restore backup bundle: %v", rollbackErr)
		}
		return 1
	}
	logf("replacement app bundle launched")
	_ = os.RemoveAll(staging)
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
