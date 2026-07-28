package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

const configSnapshotRetention = 5

type ConfigSnapshot struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	SourcePath    string `json:"sourcePath"`
	RecordedAt    string `json:"recordedAt"`
	Version       string `json:"version,omitempty"`
}

func snapshotDir() string {
	if root := config.MemoryUserDir(); root != "" {
		return filepath.Join(root, "repair", "snapshots")
	}
	return ""
}

func recordConfigSnapshot(source string, b []byte, version string, now time.Time) error {
	dir := snapshotDir()
	if dir == "" {
		return nil
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	existing, _ := ListConfigSnapshots()
	if len(existing) > 0 && strings.EqualFold(existing[0].SHA256, hash) {
		return nil
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	id := stamp + "-" + hash[:12]
	path := filepath.Join(dir, id+".toml")
	if err := fileutil.AtomicWriteFile(path, b, 0o600); err != nil {
		return err
	}
	meta := ConfigSnapshot{SchemaVersion: 1, ID: id, Path: path, SHA256: hash, SourcePath: source, RecordedAt: now.UTC().Format(time.RFC3339Nano), Version: version}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(path+".json", append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return pruneConfigSnapshots(configSnapshotRetention)
}

func ListConfigSnapshots() ([]ConfigSnapshot, error) {
	dir := snapshotDir()
	if dir == "" {
		return []ConfigSnapshot{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ConfigSnapshot{}, nil
		}
		return nil, err
	}
	out := make([]ConfigSnapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var snap ConfigSnapshot
		if json.Unmarshal(b, &snap) != nil || validateConfigSnapshot(dir, &snap) != nil {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt > out[j].RecordedAt })
	return out, nil
}

func RestoreConfigSnapshot(id string) (*RepairTransaction, error) {
	dest := config.UserConfigPath()
	if dest == "" {
		return nil, fmt.Errorf("global config path is unavailable")
	}
	unlockTransaction, err := lockRepairTransaction()
	if err != nil {
		return nil, err
	}
	defer unlockTransaction()
	unlock, err := lockRepairMutations(dest)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return restoreConfigSnapshotUnlocked(id)
}

func restoreConfigSnapshotUnlocked(id string) (*RepairTransaction, error) {
	return restoreConfigSnapshotBoundUnlocked(id, nil, nil)
}

func restoreConfigSnapshotBoundUnlocked(id string, expectedStates map[string]string, confirmedSnapshot []byte) (*RepairTransaction, error) {
	snapshots, err := ListConfigSnapshots()
	if err != nil {
		return nil, err
	}
	var selected *ConfigSnapshot
	for i := range snapshots {
		if snapshots[i].ID == id {
			selected = &snapshots[i]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("config snapshot %q not found", id)
	}
	if expectedStates == nil {
		if err := verifyConfigSnapshot(*selected); err != nil {
			return nil, err
		}
	} else {
		if err := verifyConfirmedConfigSnapshot(*selected, confirmedSnapshot); err != nil {
			return nil, err
		}
	}
	dest := config.UserConfigPath()
	if dest == "" {
		return nil, fmt.Errorf("global config path is unavailable")
	}
	if err := verifyRepairPlanFileState(dest, expectedStates); err != nil {
		return nil, err
	}
	if err := verifyRepairPlanFileState(selected.Path, expectedStates); err != nil {
		return nil, err
	}
	repairMutationBeforeRename(dest)
	if err := verifyRepairPlanFileState(dest, expectedStates); err != nil {
		return nil, err
	}
	if err := verifyRepairPlanFileState(selected.Path, expectedStates); err != nil {
		return nil, err
	}
	tx := newRepairTransaction(time.Now())
	backup := filepath.Join(config.MemoryUserDir(), "repair", "restore-backups", tx.ID+".toml")
	if expectedStates != nil {
		backup = dest + ".reasonix-restore-" + tx.ID
	}
	moved := false
	if info, err := os.Lstat(dest); err == nil {
		// Move the live file aside instead of copying its bytes: dest may be a
		// symlink (a dotfiles-managed config), and a byte copy would record a
		// plain file, so undo could never restore the link. Rename preserves
		// the exact node; undo recreates symlinks from the quarantined link.
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return nil, err
		}
		if renameErr := snapshotRename(dest, backup); renameErr == nil {
			moved = true
			repairMutationAfterRename(dest)
		} else if expectedStates != nil {
			return nil, fmt.Errorf("restore config snapshot: move confirmed config: %w", renameErr)
		} else if info.Mode()&os.ModeSymlink != 0 {
			// Cross-device fallback: recreate the link node at the backup path
			// so undo still restores a symlink, then drop the original.
			linkTarget, err := os.Readlink(dest)
			if err != nil {
				return nil, err
			}
			if err := os.Symlink(linkTarget, backup); err != nil {
				return nil, err
			}
			if err := os.Remove(dest); err != nil {
				_ = os.Remove(backup)
				return nil, err
			}
			moved = true
		} else {
			current, err := os.ReadFile(dest)
			if err != nil {
				return nil, err
			}
			if err := fileutil.AtomicWriteFile(backup, current, 0o600); err != nil {
				return nil, err
			}
		}
		if moved {
			if expectedStates != nil {
				if _, err := os.Lstat(dest); err == nil {
					return nil, fmt.Errorf("repair plan preview changed since confirmation; target was recreated during snapshot restore; confirmed state remains at %s", backup)
				} else if !os.IsNotExist(err) {
					return nil, err
				}
			}
			if expected := expectedStates[dest]; expected != "" {
				if err := verifyRepairPlanStateID(backup, expected); err != nil {
					return nil, joinRestoreCleanupError(err, backup, restoreRepairNodeIfAbsent(backup, dest))
				}
			}
		}
		tx.Changes = append(tx.Changes, RepairChange{Scope: "global", TargetPath: dest, PreviousPath: backup})
	} else if os.IsNotExist(err) {
		tx.Changes = append(tx.Changes, RepairChange{Scope: "global", TargetPath: dest, RemoveOnUndo: true})
	} else {
		return nil, err
	}
	b := confirmedSnapshot
	if expectedStates == nil {
		b, err = os.ReadFile(selected.Path)
		if err != nil {
			if moved {
				err = joinRestoreCleanupError(err, backup, restoreBackupNode(backup, dest))
			}
			return nil, err
		}
	}
	writeSnapshot := fileutil.AtomicWriteFile
	if expectedStates != nil {
		writeSnapshot = fileutil.AtomicCreateFile
	}
	if err := writeSnapshot(dest, b, 0o600); err != nil {
		if moved {
			if _, statErr := os.Lstat(dest); os.IsNotExist(statErr) {
				err = joinRestoreCleanupError(err, backup, restoreBackupNode(backup, dest))
			} else if saveErr := saveRepairTransaction(tx); saveErr != nil {
				err = fmt.Errorf("%w; preserve displaced config transaction: %v", err, saveErr)
			}
		}
		return nil, err
	}
	if err := saveRepairTransaction(tx); err != nil {
		if tx.Changes[0].RemoveOnUndo {
			_ = os.Remove(dest)
			return nil, err
		}
		return nil, joinRestoreCleanupError(err, backup, restoreBackupNode(backup, dest))
	}
	return tx, nil
}

func verifyConfirmedConfigSnapshot(snap ConfigSnapshot, content []byte) error {
	if err := config.ValidateBytes(content); err != nil {
		return fmt.Errorf("config snapshot %q is invalid: %w", snap.ID, err)
	}
	sum := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), snap.SHA256) {
		return fmt.Errorf("config snapshot %q hash mismatch", snap.ID)
	}
	return nil
}

// restoreBackupNode puts the backup node back at dest, replacing whatever sits
// there, without a window where dest is missing. Rename is tried first; when it
// fails (notably across filesystems, where the backup lives in the state dir),
// the node is recreated by type — a symlink is rebuilt beside dest and renamed
// into place, a regular file is rewritten atomically. The backup is consumed
// only after dest holds the restored node, so a failure never loses the config.
func restoreBackupNode(backup, dest string) error {
	if snapshotRename(backup, dest) == nil {
		return nil
	}
	info, err := os.Lstat(backup)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(backup)
		if err != nil {
			return err
		}
		tmp := dest + ".reasonix-restore-tmp"
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		_ = os.Remove(backup)
		return nil
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(dest, b, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Remove(backup)
	return nil
}

// snapshotRename is an indirection over os.Rename so tests can force the
// cross-device fallback paths.
var snapshotRename = os.Rename

func joinRestoreCleanupError(err error, backup string, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("restore original config from %s: %w", backup, cleanupErr))
}

func validateConfigSnapshot(dir string, snap *ConfigSnapshot) error {
	if snap == nil || snap.SchemaVersion != 1 || snap.ID == "" || snap.Path == "" || snap.SHA256 == "" {
		return fmt.Errorf("snapshot metadata is incomplete")
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(snap.Path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("snapshot path is outside snapshot directory")
	}
	if filepath.Base(snap.Path) != snap.ID+".toml" {
		return fmt.Errorf("snapshot id does not match path")
	}
	return nil
}

func verifyConfigSnapshot(snap ConfigSnapshot) error {
	if err := config.ValidateFile(snap.Path); err != nil {
		return err
	}
	got, err := hashFile(snap.Path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, snap.SHA256) {
		return fmt.Errorf("snapshot %s failed SHA-256 verification", snap.ID)
	}
	return nil
}

func pruneConfigSnapshots(keep int) error {
	snapshots, err := ListConfigSnapshots()
	if err != nil {
		return err
	}
	for _, snap := range snapshots[minimum(keep, len(snapshots)):] {
		_ = os.Remove(snap.Path)
		_ = os.Remove(snap.Path + ".json")
	}
	return nil
}

func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}
