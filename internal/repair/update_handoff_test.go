package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prepareTestAppBundleHandoff(t *testing.T) (*UpdateTransaction, string) {
	t.Helper()
	t.Setenv("REASONIX_HOME", t.TempDir())

	installRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(installRoot, "Reasonix.app")
	exe := filepath.Join(app, "Contents", "MacOS", "Reasonix")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })

	staging, err := os.MkdirTemp("", "reasonix-mac-update-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })
	stagedApp := filepath.Join(staging, "Reasonix.app")
	if err := os.MkdirAll(stagedApp, 0o700); err != nil {
		t.Fatal(err)
	}
	tx, err := PrepareAppBundleUpdateHandoff(
		"v1",
		"v2",
		app,
		app+".reasonix-update-backup",
		stagedApp,
		staging,
		os.Getpid(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return tx, staging
}

func TestClaimPendingAppBundleUpdateHandoffReturnsRecordedPaths(t *testing.T) {
	tx, _ := prepareTestAppBundleHandoff(t)
	var lockedPaths []string
	originalBeforeLock := repairMutationBeforeLock
	repairMutationBeforeLock = func(paths []string) {
		lockedPaths = append([]string(nil), paths...)
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalBeforeLock })

	claimed, release, err := ClaimPendingAppBundleUpdateHandoff(tx.ToVersion, tx.CreatedAt, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TargetPath != tx.TargetPath ||
		claimed.BackupPath != tx.BackupPath ||
		claimed.HandoffAppPath != tx.HandoffAppPath ||
		claimed.HandoffStagingPath != tx.HandoffStagingPath ||
		claimed.HandoffOwnerPID != tx.HandoffOwnerPID {
		release()
		t.Fatalf("claim returned different paths: %#v", claimed)
	}
	if len(lockedPaths) != 2 {
		release()
		t.Fatalf("claim locked %d paths, want target and backup: %v", len(lockedPaths), lockedPaths)
	}
	if err := ClearClaimedAppBundleUpdateHandoff(claimed); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(PendingUpdatePath()); !os.IsNotExist(err) {
		t.Fatalf("pending transaction still exists: %v", err)
	}
}

func TestClaimPendingAppBundleUpdateHandoffRejectsUnboundTarget(t *testing.T) {
	tx, staging := prepareTestAppBundleHandoff(t)
	arbitrary := filepath.Join(t.TempDir(), "Unrelated.app")
	if err := os.MkdirAll(arbitrary, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(arbitrary, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx.TargetPath = arbitrary
	tx.BackupPath = arbitrary + ".reasonix-update-backup"
	tx.HandoffAppPath = filepath.Join(staging, "Other.app")
	if err := WritePendingUpdate(tx); err != nil {
		t.Fatal(err)
	}

	if _, release, err := ClaimPendingAppBundleUpdateHandoff(tx.ToVersion, tx.CreatedAt, time.Second); err == nil {
		release()
		t.Fatal("unbound app target was claimed")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("unbound target changed: %q, %v", got, err)
	}
}

func TestClaimPendingAppBundleUpdateHandoffRejectsLegacyTransaction(t *testing.T) {
	tx, _ := prepareTestAppBundleHandoff(t)
	tx.HandoffAppPath = ""
	tx.HandoffStagingPath = ""
	tx.HandoffOwnerPID = 0
	if err := WritePendingUpdate(tx); err != nil {
		t.Fatal(err)
	}

	_, release, err := ClaimPendingAppBundleUpdateHandoff(tx.ToVersion, tx.CreatedAt, time.Second)
	if release != nil {
		release()
	}
	if err == nil || !strings.Contains(err.Error(), "handoff metadata is missing") {
		t.Fatalf("claim error = %v, want legacy transaction rejection", err)
	}
	if _, err := ReadPendingUpdate(); err != nil {
		t.Fatalf("legacy transaction should remain readable: %v", err)
	}
}

func TestClaimPendingAppBundleUpdateHandoffRejectsReplacementWhileLocking(t *testing.T) {
	tx, staging := prepareTestAppBundleHandoff(t)
	replacement := *tx
	replacement.HandoffAppPath = filepath.Join(staging, "Replacement.app")

	originalBeforeLock := repairMutationBeforeLock
	changed := false
	repairMutationBeforeLock = func([]string) {
		if changed {
			return
		}
		changed = true
		if err := WritePendingUpdate(&replacement); err != nil {
			t.Errorf("replace pending transaction: %v", err)
		}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalBeforeLock })

	_, release, err := ClaimPendingAppBundleUpdateHandoff(tx.ToVersion, tx.CreatedAt, time.Second)
	if release != nil {
		release()
	}
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("claim error = %v, want replacement rejection", err)
	}
}
