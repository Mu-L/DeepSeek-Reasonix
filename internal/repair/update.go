package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
)

const updateTransactionVersion = 1
const pendingUpdateLockTimeout = 5 * time.Second

var repairExecutable = os.Executable

type UpdateTransaction struct {
	SchemaVersion int    `json:"schemaVersion"`
	FromVersion   string `json:"fromVersion,omitempty"`
	ToVersion     string `json:"toVersion"`
	Platform      string `json:"platform"`
	TargetKind    string `json:"targetKind"` // file | app-bundle
	TargetPath    string `json:"targetPath"`
	BackupPath    string `json:"backupPath"`
	BackupSHA256  string `json:"backupSha256,omitempty"`
	// Files lists every binary of the release unit the update replaces
	// (main executable first, then Guard/launcher siblings). Rollback must
	// restore all of them together: restoring only the main binary would
	// leave a mixed old-desktop/new-Guard install. Empty on transactions
	// recorded by kinds that back up a single unit (macOS app bundles).
	Files     []UpdateTransactionFile `json:"files,omitempty"`
	CreatedAt string                  `json:"createdAt"`
	// Handoff fields authorize the detached macOS updater to act on paths
	// recorded by the live desktop process. They are optional so pending
	// transactions written by older releases remain readable.
	HandoffAppPath     string `json:"handoffAppPath,omitempty"`
	HandoffStagingPath string `json:"handoffStagingPath,omitempty"`
	HandoffAppTreeID   string `json:"handoffAppTreeId,omitempty"`
	HandoffOwnerPID    int    `json:"handoffOwnerPid,omitempty"`
	// BackupTreeID binds a macOS rollback backup to the bundle captured before
	// the update. It remains optional for legacy transactions.
	BackupTreeID string `json:"backupTreeId,omitempty"`
}

type UpdateTransactionFile struct {
	TargetPath    string `json:"targetPath"`
	BackupPath    string `json:"backupPath,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	MissingBefore bool   `json:"missingBefore,omitempty"`
}

type UpdateRollbackResult struct {
	RolledBack  bool   `json:"rolledBack"`
	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	TargetPath  string `json:"targetPath,omitempty"`
	// MixedInstall reports that a failed rollback could not be compensated:
	// the install now mixes binaries from two releases. Launchers must not
	// start the desktop in this state.
	MixedInstall bool `json:"mixedInstall,omitempty"`
}

func PendingUpdatePath() string {
	root := config.MemoryUserDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "repair", "pending-update.json")
}

// lockPendingUpdateStrict serializes cross-process pending-update transitions:
// prepare, rollback, commit, and cancel. Two launchers can run recovery at
// once — a failed update makes startup slow, so a double-clicked Guard is
// realistic — and restoreReleaseUnit's fixed staging/aside paths assume a
// single restorer; unserialized, the loser's compensation can re-install the
// new binaries over the winner's completed rollback.
func lockPendingUpdateStrict() (func(), error) {
	path := PendingUpdatePath()
	if path == "" {
		return nil, fmt.Errorf("pending update: Reasonix state directory is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingUpdateLockTimeout)
	defer cancel()
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	return unlock, nil
}

var acquirePendingUpdateLock = lockPendingUpdateStrict

// PrepareFileUpdate snapshots the current desktop executable — plus any sibling
// binaries of the release unit the installer also replaces (Guard, launcher,
// update helper) — and records an update transaction before an updater applies
// the replacement. Sibling paths that do not exist are recorded explicitly so
// rollback can remove files introduced by the replacement release.
func PrepareFileUpdate(fromVersion, toVersion, targetPath string, siblingPaths ...string) (*UpdateTransaction, error) {
	targetPath = filepath.Clean(strings.TrimSpace(targetPath))
	if targetPath == "" || targetPath == "." {
		return nil, fmt.Errorf("prepare update: empty target path")
	}
	root := config.MemoryUserDir()
	if root == "" {
		return nil, fmt.Errorf("prepare update: Reasonix state directory is unavailable")
	}
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	// Hold the same target locks as rollback so prepare/snapshot cannot race
	// a concurrent Guard restore of the release unit.
	lockPaths := append([]string{targetPath}, siblingPaths...)
	unlockTargets, lockErr := lockRepairMutations(lockPaths...)
	if lockErr != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	backupDir := filepath.Join(root, "repair", "updates")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, err
	}
	if !pathInsideResolvedRoot(filepath.Join(root, "repair"), backupDir) {
		return nil, fmt.Errorf("prepare update: backup directory resolves outside the repair directory")
	}
	tx := &UpdateTransaction{
		SchemaVersion: updateTransactionVersion,
		FromVersion:   fromVersion,
		ToVersion:     toVersion,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		TargetKind:    "file",
		TargetPath:    targetPath,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	seen := map[string]bool{}
	for i, path := range append([]string{targetPath}, siblingPaths...) {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "" || path == "." || seen[path] {
			continue
		}
		seen[path] = true
		if i > 0 {
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					tx.Files = append(tx.Files, UpdateTransactionFile{TargetPath: path, MissingBefore: true})
					continue
				}
				return nil, fmt.Errorf("prepare update backup: %w", err)
			}
		}
		backupPath := filepath.Join(backupDir, filepath.Base(path)+".previous")
		hash, err := copyFileWithHash(path, backupPath, 0o700)
		if err != nil {
			return nil, fmt.Errorf("prepare update backup: %w", err)
		}
		tx.Files = append(tx.Files, UpdateTransactionFile{TargetPath: path, BackupPath: backupPath, SHA256: hash})
		if i == 0 {
			tx.BackupPath = backupPath
			tx.BackupSHA256 = hash
		}
	}
	if err := WritePendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// PrepareAppBundleUpdate records the sibling bundle backup that the macOS
// handoff script creates. The script performs the directory move after exit.
func PrepareAppBundleUpdate(fromVersion, toVersion, appPath, backupPath string) (*UpdateTransaction, error) {
	tx, err := newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath)
	if err != nil {
		return nil, err
	}
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	unlockTargets, lockErr := lockRepairMutations(tx.TargetPath, tx.BackupPath)
	if lockErr != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	tx.BackupTreeID, err = repairPlanTreeContentStateID(tx.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: current bundle digest: %w", err)
	}
	if err := WritePendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// PrepareAppBundleUpdateHandoff records every path the detached macOS updater
// may mutate. The child receives only the transaction identity and must claim
// these recorded paths under the pending-update and mutation locks.
func PrepareAppBundleUpdateHandoff(fromVersion, toVersion, appPath, backupPath, stagedAppPath, stagingPath string, ownerPID int) (*UpdateTransaction, error) {
	tx, err := newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(tx.TargetPath) {
		return nil, fmt.Errorf("prepare update: invalid macOS bundle paths")
	}
	tx.HandoffAppPath = filepath.Clean(strings.TrimSpace(stagedAppPath))
	tx.HandoffStagingPath = filepath.Clean(strings.TrimSpace(stagingPath))
	tx.HandoffOwnerPID = ownerPID
	if err := validateAppBundleHandoffMetadata(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}

	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	unlockTargets, err := lockRepairMutations(tx.TargetPath, tx.BackupPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", err)
	}
	defer unlockTargets()
	tx.HandoffAppTreeID, err = repairPlanTreeContentStateID(tx.HandoffAppPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: stage bundle digest: %w", err)
	}
	tx.BackupTreeID, err = repairPlanTreeContentStateID(tx.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: current bundle digest: %w", err)
	}
	if err := WritePendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath string) (*UpdateTransaction, error) {
	tx := &UpdateTransaction{
		SchemaVersion: updateTransactionVersion,
		FromVersion:   fromVersion,
		ToVersion:     toVersion,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		TargetKind:    "app-bundle",
		TargetPath:    filepath.Clean(strings.TrimSpace(appPath)),
		BackupPath:    filepath.Clean(strings.TrimSpace(backupPath)),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !strings.HasSuffix(strings.ToLower(tx.TargetPath), ".app") ||
		tx.BackupPath != tx.TargetPath+".reasonix-update-backup" {
		return nil, fmt.Errorf("prepare update: invalid macOS bundle paths")
	}
	return tx, nil
}

// ClaimPendingAppBundleUpdateHandoff authorizes a detached child to perform the
// recorded bundle swap. It returns with both the pending transaction lock and
// the target mutation locks held; release must be called on every path.
func ClaimPendingAppBundleUpdateHandoff(expectedToVersion, expectedCreatedAt string, timeout time.Duration) (*UpdateTransaction, func(), error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedToVersion == "" || expectedCreatedAt == "" {
		return nil, nil, fmt.Errorf("claim update handoff: transaction identity is incomplete")
	}
	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, nil, fmt.Errorf("claim update handoff: lock pending transaction: %w", err)
	}
	fail := func(err error) (*UpdateTransaction, func(), error) {
		unlockPending()
		return nil, nil, err
	}

	tx, err := ReadPendingUpdate()
	if err != nil {
		return fail(fmt.Errorf("claim update handoff: read pending transaction: %w", err))
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return fail(fmt.Errorf("claim update handoff: pending transaction does not match"))
	}
	if tx.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return fail(fmt.Errorf("claim update handoff: pending transaction platform does not match"))
	}
	if err := validateAppBundleHandoffMetadata(tx); err != nil {
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}
	if strings.TrimSpace(tx.HandoffAppPath) == "" ||
		strings.TrimSpace(tx.HandoffStagingPath) == "" ||
		tx.HandoffOwnerPID <= 0 {
		return fail(fmt.Errorf("claim update handoff: handoff metadata is missing"))
	}

	unlockTargets, err := lockRepairMutationsTimeout(timeout, tx.TargetPath, tx.BackupPath)
	if err != nil {
		return fail(fmt.Errorf("claim update handoff: lock targets: %w", err))
	}
	current, err := ReadPendingUpdate()
	if err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: re-read pending transaction: %w", err))
	}
	if !reflect.DeepEqual(tx, current) {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: pending transaction changed while waiting"))
	}
	if err := VerifyAppBundleUpdateHandoffSource(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			unlockTargets()
			unlockPending()
		})
	}
	return current, release, nil
}

// ClaimPendingFileUpdate binds an updater's actual replacement window to the
// exact transaction and release-unit paths prepared by the desktop. The
// launcher path is explicit because the Windows helper runs from a cache
// directory rather than from the installation it is authorized to replace.
func ClaimPendingFileUpdate(
	expectedToVersion, expectedCreatedAt, launcherPath string,
	expectedTargetPaths []string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	launcherPath = filepath.Clean(strings.TrimSpace(launcherPath))
	if expectedToVersion == "" || expectedCreatedAt == "" || launcherPath == "" || launcherPath == "." {
		return nil, nil, fmt.Errorf("claim file update: transaction identity is incomplete")
	}
	if len(expectedTargetPaths) == 0 {
		return nil, nil, fmt.Errorf("claim file update: release unit is empty")
	}

	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, nil, fmt.Errorf("claim file update: lock pending transaction: %w", err)
	}
	fail := func(err error) (*UpdateTransaction, func(), error) {
		unlockPending()
		return nil, nil, err
	}
	tx, err := readPendingUpdateForLauncher(launcherPath)
	if err != nil {
		return fail(fmt.Errorf("claim file update: read pending transaction: %w", err))
	}
	if tx.TargetKind != "file" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return fail(fmt.Errorf("claim file update: pending transaction does not match"))
	}
	if tx.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return fail(fmt.Errorf("claim file update: pending transaction platform does not match"))
	}
	targetPaths := pendingUpdateTargetPaths(tx)
	if !sameRepairMutationPaths(targetPaths, expectedTargetPaths) {
		return fail(fmt.Errorf("claim file update: release unit does not match"))
	}

	unlockTargets, err := lockRepairMutationsTimeout(timeout, targetPaths...)
	if err != nil {
		return fail(fmt.Errorf("claim file update: lock targets: %w", err))
	}
	current, err := readPendingUpdateForLauncher(launcherPath)
	if err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim file update: re-read pending transaction: %w", err))
	}
	if !reflect.DeepEqual(tx, current) {
		unlockTargets()
		return fail(fmt.Errorf("claim file update: pending transaction changed while waiting"))
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			unlockTargets()
			unlockPending()
		})
	}
	return current, release, nil
}

// CancelPendingAppBundleUpdateHandoff abandons an exact handoff only when the
// original installed bundle is still the tree captured during prepare. This is
// the safe recovery path when source verification fails after the desktop has
// exited but before any bundle swap occurred.
func CancelPendingAppBundleUpdateHandoff(
	expectedToVersion, expectedCreatedAt string,
	timeout time.Duration,
) (*UpdateTransaction, error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedToVersion == "" || expectedCreatedAt == "" {
		return nil, fmt.Errorf("cancel update handoff: transaction identity is incomplete")
	}
	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: lock pending transaction: %w", err)
	}
	defer unlockPending()
	tx, err := ReadPendingUpdate()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: read pending transaction: %w", err)
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return nil, fmt.Errorf("cancel update handoff: pending transaction does not match")
	}
	unlockTargets, err := lockRepairMutationsTimeout(timeout, tx.TargetPath, tx.BackupPath)
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: lock targets: %w", err)
	}
	defer unlockTargets()
	current, err := ReadPendingUpdate()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(tx, current) {
		return nil, fmt.Errorf("cancel update handoff: pending transaction changed while waiting")
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
		return nil, fmt.Errorf("cancel update handoff: %w", err)
	}
	if err := os.Remove(PendingUpdatePath()); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	removeUpdateBackups(current)
	return current, nil
}

func sameRepairMutationPaths(a, b []string) bool {
	keys := func(paths []string) []string {
		seen := make(map[string]struct{}, len(paths))
		result := make([]string, 0, len(paths))
		for _, path := range paths {
			key := canonicalRepairPath(path)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, key)
		}
		sort.Strings(result)
		return result
	}
	return reflect.DeepEqual(keys(a), keys(b))
}

// VerifyAppBundleUpdateHandoffSource checks the real staging containment and
// the complete staged tree immediately before a handoff mutates the install.
// The lexical metadata check remains readable after staging cleanup, while this
// stronger check is only used while the source bundle still exists.
func VerifyAppBundleUpdateHandoffSource(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff source transaction is invalid")
	}
	if strings.TrimSpace(tx.HandoffAppTreeID) == "" {
		return fmt.Errorf("handoff source digest is missing")
	}
	if err := validateAppBundleHandoffSourcePaths(tx); err != nil {
		return err
	}
	actual, err := repairPlanTreeContentStateID(tx.HandoffAppPath)
	if err != nil {
		return fmt.Errorf("read staged bundle digest: %w", err)
	}
	if actual != tx.HandoffAppTreeID {
		return fmt.Errorf("staged bundle changed after verification")
	}
	return nil
}

// VerifyAppBundleUpdateHandoffTarget proves that the bytes copied into the
// installed bundle are the same tree that was verified in staging.
func VerifyAppBundleUpdateHandoffTarget(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff target transaction is invalid")
	}
	if strings.TrimSpace(tx.HandoffAppTreeID) == "" {
		return fmt.Errorf("handoff target digest is missing")
	}
	actual, err := repairPlanTreeContentStateID(tx.TargetPath)
	if err != nil {
		return fmt.Errorf("read installed bundle digest: %w", err)
	}
	if actual != tx.HandoffAppTreeID {
		return fmt.Errorf("installed bundle differs from verified staging")
	}
	return nil
}

// VerifyAppBundleUpdateHandoffOriginal checks that the installed bundle about
// to become the rollback backup is still the tree captured during prepare.
func VerifyAppBundleUpdateHandoffOriginal(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff original transaction is invalid")
	}
	return verifyAppBundleUpdateTree(tx.TargetPath, tx.BackupTreeID, "installed bundle changed after prepare")
}

// VerifyAppBundleUpdateHandoffBackup checks the node produced by the
// target-to-backup rename before the replacement bundle is copied into place.
func VerifyAppBundleUpdateHandoffBackup(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff backup transaction is invalid")
	}
	return verifyAppBundleUpdateTree(tx.BackupPath, tx.BackupTreeID, "rollback backup differs from prepared bundle")
}

func verifyAppBundleUpdateTree(path, expected, mismatch string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("original bundle digest is missing")
	}
	actual, err := repairPlanTreeContentStateID(path)
	if err != nil {
		return fmt.Errorf("read original bundle digest: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("%s", mismatch)
	}
	return nil
}

// AppBundleTreeDigest exposes the deterministic bundle-content digest to the
// desktop handoff tests and other platform glue without exposing path identity.
func AppBundleTreeDigest(path string) (string, error) {
	return repairPlanTreeContentStateID(path)
}

func validateAppBundleHandoffSourcePaths(tx *UpdateTransaction) error {
	staging, err := filepath.EvalSymlinks(tx.HandoffStagingPath)
	if err != nil {
		return fmt.Errorf("resolve handoff staging directory: %w", err)
	}
	app, err := filepath.EvalSymlinks(tx.HandoffAppPath)
	if err != nil {
		return fmt.Errorf("resolve handoff app bundle: %w", err)
	}
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return fmt.Errorf("resolve temporary directory: %w", err)
	}
	within := func(root, path string) bool {
		rel, relErr := filepath.Rel(root, path)
		return relErr == nil && rel != "." && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !within(tempRoot, staging) {
		return fmt.Errorf("handoff staging directory resolves outside the system temporary directory")
	}
	if !within(staging, app) {
		return fmt.Errorf("handoff app bundle resolves outside its staging directory")
	}
	if info, statErr := os.Stat(app); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return fmt.Errorf("handoff app bundle is unavailable: %w", statErr)
		}
		return fmt.Errorf("handoff app bundle is not a directory")
	}
	return nil
}

// ClearClaimedAppBundleUpdateHandoff removes a failed handoff transaction.
// The caller must still hold the claim returned above.
func ClearClaimedAppBundleUpdateHandoff(claimed *UpdateTransaction) error {
	current, err := ReadPendingUpdate()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(claimed, current) {
		return fmt.Errorf("clear update handoff: pending transaction changed")
	}
	if err := os.Remove(PendingUpdatePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func WritePendingUpdate(tx *UpdateTransaction) error {
	if tx == nil {
		return fmt.Errorf("pending update: nil transaction")
	}
	path := PendingUpdatePath()
	if path == "" {
		return fmt.Errorf("pending update: Reasonix state directory is unavailable")
	}
	b, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(path, append(b, '\n'), 0o600)
}

func ReadPendingUpdate() (*UpdateTransaction, error) {
	tx, err := readPendingUpdateUnchecked()
	if err != nil {
		return nil, err
	}
	if err := validateUpdateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func readPendingUpdateForLauncher(launcherPath string) (*UpdateTransaction, error) {
	tx, err := readPendingUpdateUnchecked()
	if err != nil {
		return nil, err
	}
	if err := validateUpdateTransactionForLauncher(tx, launcherPath); err != nil {
		return nil, err
	}
	return tx, nil
}

func readPendingUpdateUnchecked() (*UpdateTransaction, error) {
	path := PendingUpdatePath()
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tx UpdateTransaction
	if err := json.Unmarshal(b, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func HasPendingUpdate() bool {
	_, err := ReadPendingUpdate()
	return err == nil
}

// MarkUpdateHealthy commits a probationary update and removes its backup. A
// version mismatch is ignored so an older process cannot bless a newer update.
func MarkUpdateHealthy(runningVersion string) error {
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return fmt.Errorf("mark update healthy: lock pending transaction: %w", err)
	}
	defer unlock()
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(runningVersion) != strings.TrimSpace(tx.ToVersion) {
		return nil
	}
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return fmt.Errorf("mark update healthy: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	if err := os.Remove(PendingUpdatePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeUpdateBackups(tx)
	return nil
}

// CancelPendingUpdate removes a transaction that failed before control was
// handed to the replacement build. A version mismatch is intentionally inert.
func CancelPendingUpdate(toVersion string) error {
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return fmt.Errorf("cancel pending update: lock pending transaction: %w", err)
	}
	defer unlock()
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(toVersion) != strings.TrimSpace(tx.ToVersion) {
		return nil
	}
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return fmt.Errorf("cancel pending update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	if err := os.Remove(PendingUpdatePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeUpdateBackups(tx)
	return nil
}

func removeUpdateBackups(tx *UpdateTransaction) {
	if tx == nil {
		return
	}
	if tx.TargetKind == "app-bundle" {
		if tx.BackupPath != "" {
			_ = os.RemoveAll(tx.BackupPath)
		}
		return
	}
	if tx.BackupPath != "" {
		_ = os.Remove(tx.BackupPath)
	}
	for _, f := range tx.Files {
		if f.BackupPath != "" {
			_ = os.Remove(f.BackupPath)
		}
	}
}

func RollbackPendingUpdate() (UpdateRollbackResult, error) {
	return rollbackPendingUpdate("", "")
}

func rollbackPendingUpdate(expectedToVersion, expectedCreatedAt string) (UpdateRollbackResult, error) {
	return rollbackPendingUpdateMatching(expectedToVersion, expectedCreatedAt, "")
}

func rollbackPendingUpdateState(expectedStateID string) (UpdateRollbackResult, error) {
	return rollbackPendingUpdateMatching("", "", expectedStateID)
}

func rollbackPendingUpdateMatching(expectedToVersion, expectedCreatedAt, expectedStateID string) (UpdateRollbackResult, error) {
	// The expected-match checks below re-run under the strict lock, so a
	// transaction committed, cancelled, or replaced while waiting here is never
	// acted upon.
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: lock pending transaction: %w", err)
	}
	defer unlock()
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return UpdateRollbackResult{}, nil
		}
		return UpdateRollbackResult{}, err
	}
	if expected := strings.TrimSpace(expectedToVersion); expected != "" && expected != strings.TrimSpace(tx.ToVersion) {
		return UpdateRollbackResult{}, nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return UpdateRollbackResult{}, nil
	}
	// Share release-unit target locks with other repair mutations so two
	// REASONIX_HOME profiles cannot quarantine or restore the same binaries
	// through different pending-update locks.
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	if expected := strings.TrimSpace(expectedStateID); expected != "" {
		actual, _ := pendingUpdateBoundPreview(tx)
		if expected != actual {
			return UpdateRollbackResult{}, nil
		}
	}
	result := UpdateRollbackResult{FromVersion: tx.ToVersion, ToVersion: tx.FromVersion, TargetPath: tx.TargetPath}
	switch tx.TargetKind {
	case "file":
		files := pendingUpdateFiles(tx)
		// Verify every backup before touching any binary: a partial restore
		// would recreate exactly the mixed-version install rollback exists to
		// prevent. A missing hash is a validation failure, not a bypass —
		// ReadPendingUpdate already rejects hashless file transactions, so
		// this guards hand-crafted callers.
		for _, f := range files {
			if f.MissingBefore {
				continue
			}
			if strings.TrimSpace(f.SHA256) == "" {
				return result, fmt.Errorf("rollback update: backup hash missing for %s", filepath.Base(f.TargetPath))
			}
			got, hashErr := hashFile(f.BackupPath)
			if hashErr != nil || !strings.EqualFold(got, f.SHA256) {
				return result, fmt.Errorf("rollback update: backup hash mismatch for %s", filepath.Base(f.TargetPath))
			}
		}
		mixed, restoreErr := restoreReleaseUnit(files)
		if restoreErr != nil {
			result.MixedInstall = mixed
			return result, fmt.Errorf("rollback update: %w", restoreErr)
		}
	case "app-bundle":
		if _, err := os.Stat(tx.BackupPath); err != nil {
			return result, fmt.Errorf("rollback update: backup bundle: %w", err)
		}
		if tx.BackupTreeID != "" {
			actual, digestErr := repairPlanTreeContentStateID(tx.BackupPath)
			if digestErr != nil || actual != tx.BackupTreeID {
				return result, fmt.Errorf("rollback update: backup bundle digest mismatch")
			}
		}
		failed := tx.TargetPath + ".reasonix-failed-" + time.Now().UTC().Format("20060102T150405Z")
		retainedFailed := false
		if _, statErr := os.Lstat(tx.TargetPath); statErr == nil {
			if err := os.Rename(tx.TargetPath, failed); err != nil {
				return result, fmt.Errorf("rollback update: move failed bundle: %w", err)
			}
			retainedFailed = true
		} else if !os.IsNotExist(statErr) {
			return result, fmt.Errorf("rollback update: inspect live bundle: %w", statErr)
		}
		if err := os.Rename(tx.BackupPath, tx.TargetPath); err != nil {
			if retainedFailed {
				_ = os.Rename(failed, tx.TargetPath)
			}
			return result, fmt.Errorf("rollback update: restore bundle: %w", err)
		}
		if retainedFailed {
			_ = os.RemoveAll(failed)
		}
	default:
		return result, fmt.Errorf("rollback update: unsupported target kind %q", tx.TargetKind)
	}
	result.RolledBack = true
	_ = os.Remove(PendingUpdatePath())
	return result, nil
}

// Rename/copy indirection so tests can inject mid-unit failures.
var (
	rollbackStageCopy  = copyFileWithHash
	rollbackSwapRename = os.Rename
)

// restoreReleaseUnit swaps every backup into place with compensation, so a
// failed rollback never leaves a mixed old/new install. Phase 1 stages each
// backup next to its target — a copy can fail halfway (disk full, unreadable
// backup) and staging keeps the live binaries untouched until every byte is
// on the target filesystem. Phase 2 swaps via renames only: each target moves
// aside first (renaming works even for the running executable, where
// overwriting does not), so a failure renames the asides back and the unit
// stays coherent on the new version for a retried rollback. Only when that
// unwinding itself fails is the install reported as mixed.
func restoreReleaseUnit(files []UpdateTransactionFile) (mixed bool, err error) {
	stages := make([]string, len(files))
	defer func() {
		for _, stage := range stages {
			if stage != "" {
				_ = os.Remove(stage)
			}
		}
	}()
	for i, f := range files {
		if f.MissingBefore {
			continue
		}
		mode := os.FileMode(0o700)
		if st, statErr := os.Stat(f.TargetPath); statErr == nil {
			mode = st.Mode().Perm()
		}
		stage := f.TargetPath + ".reasonix-rollback-stage"
		stagedSHA256, copyErr := rollbackStageCopy(f.BackupPath, stage, mode)
		if copyErr != nil {
			return false, fmt.Errorf("stage %s: %w", filepath.Base(f.TargetPath), copyErr)
		}
		stages[i] = stage
		// The backup can change after the preflight hash but before or during
		// this copy. Bind the bytes that will actually be installed, not only
		// the source path observed before staging.
		if !strings.EqualFold(stagedSHA256, f.SHA256) {
			return false, fmt.Errorf("stage %s: backup hash mismatch", filepath.Base(f.TargetPath))
		}
	}
	asides := make([]string, len(files))
	processed := make([]bool, len(files))
	restoreAttempted := make([]bool, len(files))
	failedIndex := -1
	var swapErr error
	for i, f := range files {
		aside := f.TargetPath + ".reasonix-rollback-aside"
		if renameErr := rollbackSwapRename(f.TargetPath, aside); renameErr != nil {
			if os.IsNotExist(renameErr) {
				// A rollback interrupted between renames may have consumed this
				// target while retaining the new binary at the fixed aside path.
				// Preserve that copy for compensation until the retry succeeds.
				if f.MissingBefore {
					aside = ""
				} else if _, statErr := os.Lstat(aside); statErr != nil {
					aside = ""
				}
			} else {
				failedIndex = i
				swapErr = fmt.Errorf("retain %s: %w", filepath.Base(f.TargetPath), renameErr)
				break
			}
		}
		asides[i] = aside
		if f.MissingBefore {
			// The old release did not contain this path. Retaining the new file
			// at the aside path removes it from the live release atomically; it
			// is deleted only after the whole rollback succeeds.
			processed[i] = true
			continue
		}
		restoreAttempted[i] = true
		if renameErr := rollbackSwapRename(stages[i], f.TargetPath); renameErr != nil {
			failedIndex = i
			swapErr = fmt.Errorf("restore %s: %w", filepath.Base(f.TargetPath), renameErr)
			break
		}
		stages[i] = ""
		processed[i] = true
	}
	if swapErr == nil {
		for _, f := range files {
			// Best-effort: on Windows the running executable's aside may linger
			// until the process exits, but it is no longer a live entry point.
			_ = os.Remove(f.TargetPath + ".reasonix-rollback-aside")
		}
		return false, nil
	}
	// Compensate: rename the new-version binaries back over the restored old
	// ones. A missing-before entry is compensated the same way: move the
	// retained new file back to its original path.
	for j, f := range files {
		if !processed[j] && j != failedIndex {
			continue
		}
		if asides[j] != "" {
			if rollbackSwapRename(asides[j], f.TargetPath) != nil {
				mixed = true
			}
			continue
		}
		if !f.MissingBefore && restoreAttempted[j] {
			// No retained new-version copy exists to put back after the old
			// backup was (or may have been) placed.
			mixed = true
		}
	}
	return mixed, swapErr
}

// allowedUpdateTargetBase whitelists the packaged binaries an update
// transaction may name. The main executable names are only valid as the
// primary target; Guard/launcher artifacts only as release-unit siblings.
func allowedUpdateTargetBase(base string, primary bool) bool {
	switch strings.ToLower(base) {
	case "reasonix-desktop", "reasonix-desktop.exe":
		return primary
	case "reasonix.exe":
		return true
	case "reasonix", "reasonix-guard", "reasonix-guard.exe", "reasonix-launcher.exe", "reasonix-update-helper.exe", "reasonix-cli.exe":
		return !primary
	default:
		return false
	}
}

func validateUpdateTransaction(tx *UpdateTransaction) error {
	if tx == nil || tx.SchemaVersion != updateTransactionVersion || strings.TrimSpace(tx.ToVersion) == "" {
		return fmt.Errorf("pending update metadata is incomplete")
	}
	launcher, err := repairExecutable()
	if err != nil {
		return fmt.Errorf("pending update launcher path is unavailable")
	}
	return validateUpdateTransactionForLauncher(tx, launcher)
}

func validateUpdateTransactionForLauncher(tx *UpdateTransaction, launcher string) error {
	if tx == nil || tx.SchemaVersion != updateTransactionVersion || strings.TrimSpace(tx.ToVersion) == "" {
		return fmt.Errorf("pending update metadata is incomplete")
	}
	tx.TargetPath = filepath.Clean(tx.TargetPath)
	tx.BackupPath = filepath.Clean(tx.BackupPath)
	launcher = filepath.Clean(strings.TrimSpace(launcher))
	if launcher == "" || launcher == "." {
		return fmt.Errorf("pending update launcher path is unavailable")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(launcher); resolveErr == nil {
		launcher = resolved
	}
	launcher = filepath.Clean(launcher)
	switch tx.TargetKind {
	case "file":
		if !allowedUpdateTargetBase(filepath.Base(tx.TargetPath), true) {
			return fmt.Errorf("pending update target is not a Reasonix executable")
		}
		launcherKey := canonicalRepairPath(launcher)
		targetKey := canonicalRepairPath(tx.TargetPath)
		if launcherKey == "" || targetKey == "" || filepath.Dir(launcherKey) != filepath.Dir(targetKey) {
			return fmt.Errorf("pending update target is outside the current Guard installation")
		}
		root := filepath.Clean(filepath.Join(config.MemoryUserDir(), "repair"))
		insideRepairDir := func(path string) bool {
			return pathInsideResolvedRoot(root, path)
		}
		if !insideRepairDir(tx.BackupPath) {
			return fmt.Errorf("pending update backup is outside the repair directory")
		}
		// Every restorable file must carry a hash — rollback promises to
		// verify all backups before touching any binary, so an unhashed entry
		// would silently weaken that gate.
		if strings.TrimSpace(tx.BackupSHA256) == "" {
			return fmt.Errorf("pending update backup hash is missing")
		}
		primaryListed := len(tx.Files) == 0
		for i := range tx.Files {
			f := &tx.Files[i]
			f.TargetPath = filepath.Clean(f.TargetPath)
			primary := f.TargetPath == tx.TargetPath
			primaryListed = primaryListed || primary
			if !allowedUpdateTargetBase(filepath.Base(f.TargetPath), primary) {
				return fmt.Errorf("pending update lists an unexpected release file")
			}
			if filepath.Dir(f.TargetPath) != filepath.Dir(tx.TargetPath) {
				return fmt.Errorf("pending update release file is outside the current Guard installation")
			}
			if f.MissingBefore {
				if primary || strings.TrimSpace(f.BackupPath) != "" || strings.TrimSpace(f.SHA256) != "" {
					return fmt.Errorf("pending update missing release file metadata is invalid")
				}
				continue
			}
			f.BackupPath = filepath.Clean(f.BackupPath)
			if !insideRepairDir(f.BackupPath) {
				return fmt.Errorf("pending update backup is outside the repair directory")
			}
			if strings.TrimSpace(f.SHA256) == "" {
				return fmt.Errorf("pending update release file hash is missing")
			}
		}
		if !primaryListed {
			return fmt.Errorf("pending update release unit omits the primary executable")
		}
	case "app-bundle":
		if !strings.HasSuffix(strings.ToLower(tx.TargetPath), ".app") || tx.BackupPath != tx.TargetPath+".reasonix-update-backup" {
			return fmt.Errorf("pending update bundle paths are invalid")
		}
		inside := tx.TargetPath + string(filepath.Separator)
		if !strings.HasPrefix(launcher, inside) {
			return fmt.Errorf("pending update bundle is not the current Guard installation")
		}
		if err := validateAppBundleHandoffMetadata(tx); err != nil {
			return fmt.Errorf("pending update %w", err)
		}
	default:
		return fmt.Errorf("pending update target kind is invalid")
	}
	return nil
}

func pathInsideResolvedRoot(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || path == "" {
		return false
	}
	lexicalRel, err := filepath.Rel(root, path)
	if err != nil || lexicalRel == ".." || strings.HasPrefix(lexicalRel, ".."+string(filepath.Separator)) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && resolvedRel != ".." &&
		!strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator))
}

func validateAppBundleHandoffMetadata(tx *UpdateTransaction) error {
	if tx == nil {
		return fmt.Errorf("handoff metadata is incomplete")
	}
	hasAny := strings.TrimSpace(tx.HandoffAppPath) != "" ||
		strings.TrimSpace(tx.HandoffStagingPath) != "" ||
		strings.TrimSpace(tx.HandoffAppTreeID) != "" ||
		tx.HandoffOwnerPID != 0
	if !hasAny {
		return nil
	}
	tx.HandoffAppPath = filepath.Clean(strings.TrimSpace(tx.HandoffAppPath))
	tx.HandoffStagingPath = filepath.Clean(strings.TrimSpace(tx.HandoffStagingPath))
	if tx.HandoffOwnerPID <= 0 ||
		!filepath.IsAbs(tx.HandoffAppPath) ||
		!filepath.IsAbs(tx.HandoffStagingPath) ||
		!strings.HasSuffix(strings.ToLower(tx.HandoffAppPath), ".app") {
		return fmt.Errorf("handoff metadata is incomplete")
	}
	if tx.HandoffAppPath == tx.TargetPath || tx.HandoffAppPath == tx.BackupPath {
		return fmt.Errorf("handoff app overlaps the installed bundle")
	}
	rel, err := filepath.Rel(tx.HandoffStagingPath, tx.HandoffAppPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("handoff app is outside its staging directory")
	}
	tempRoot := filepath.Clean(os.TempDir())
	stagingRel, err := filepath.Rel(tempRoot, tx.HandoffStagingPath)
	if err != nil || stagingRel == "." || stagingRel == ".." || strings.HasPrefix(stagingRel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("handoff staging directory is outside the system temporary directory")
	}
	stagingBase := strings.Split(stagingRel, string(filepath.Separator))[0]
	if !strings.HasPrefix(stagingBase, "reasonix-mac-update-") {
		return fmt.Errorf("handoff staging directory has an unexpected name")
	}
	return nil
}

func copyFileWithHash(src, dst string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".repair-copy-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := fileutil.ReplaceFile(tmpPath, dst); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
