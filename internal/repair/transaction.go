package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

type RepairChange struct {
	Scope           string `json:"scope,omitempty"`
	TargetPath      string `json:"targetPath"`
	PreviousPath    string `json:"previousPath,omitempty"`
	PreviousStateID string `json:"previousStateId,omitempty"`
	RemoveOnUndo    bool   `json:"removeOnUndo,omitempty"`
	// Undone marks a change already reverted by an interrupted undo, so a
	// retry can resume with the remaining changes instead of failing the
	// preflight on the consumed backup of a change that is already restored.
	Undone bool `json:"undone,omitempty"`
}

type RepairTransaction struct {
	SchemaVersion int            `json:"schemaVersion"`
	ID            string         `json:"id"`
	CreatedAt     string         `json:"createdAt"`
	Changes       []RepairChange `json:"changes"`
	Undone        bool           `json:"undone,omitempty"`
	UndoneAt      string         `json:"undoneAt,omitempty"`
}

func newRepairTransaction(now time.Time) *RepairTransaction {
	now = now.UTC()
	return &RepairTransaction{
		SchemaVersion: 1,
		ID:            fmt.Sprintf("repair-%d", now.UnixNano()),
		CreatedAt:     now.Format(time.RFC3339Nano),
		Changes:       []RepairChange{},
	}
}

func repairChangeForPrevious(scope, target, previous string) RepairChange {
	return RepairChange{
		Scope:           scope,
		TargetPath:      target,
		PreviousPath:    previous,
		PreviousStateID: repairPlanReleaseNodeStateFor(previous, target),
	}
}

func repairTransactionPath() string {
	if root := config.MemoryUserDir(); root != "" {
		return filepath.Join(root, "repair", "last-repair.json")
	}
	return ""
}

func repairLogPath() string {
	if root := config.MemoryUserDir(); root != "" {
		return filepath.Join(root, "repair", "repair-log.jsonl")
	}
	return ""
}

func saveRepairTransaction(tx *RepairTransaction) error {
	if tx == nil || len(tx.Changes) == 0 {
		return nil
	}
	if err := persistRepairTransaction(tx); err != nil {
		return err
	}
	appendRepairLogBestEffort(tx)
	return nil
}

// The append-only audit log is best-effort. last-repair.json is the durable
// undo state, so an audit failure must not turn an already committed filesystem
// change into a reported failure or trigger cleanup that consumes its backup.
func appendRepairLogBestEffort(tx *RepairTransaction) {
	_ = appendRepairLog(tx)
}

func persistRepairTransaction(tx *RepairTransaction) error {
	path := repairTransactionPath()
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(path, append(b, '\n'), 0o600)
}

func appendRepairLog(tx *RepairTransaction) error {
	path := repairLogPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func ReadLastRepair() (*RepairTransaction, error) {
	path := repairTransactionPath()
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tx RepairTransaction
	if err := json.Unmarshal(b, &tx); err != nil {
		return nil, err
	}
	if tx.SchemaVersion != 1 || tx.ID == "" || len(tx.Changes) == 0 {
		return nil, fmt.Errorf("last repair transaction is incomplete")
	}
	for _, change := range tx.Changes {
		if err := validateRepairChange(change); err != nil {
			return nil, err
		}
	}
	return &tx, nil
}

func validateRepairChange(change RepairChange) error {
	target := filepath.Clean(change.TargetPath)
	switch {
	case change.Scope == "global":
		if target != filepath.Clean(config.UserConfigPath()) {
			return fmt.Errorf("repair transaction global target is invalid")
		}
	case change.Scope == "project":
		if filepath.Base(target) != "reasonix.toml" {
			return fmt.Errorf("repair transaction project target is invalid")
		}
	case strings.HasPrefix(change.Scope, "derived:"):
		name := change.Scope[len("derived:"):]
		want, ok := derivedStatePaths()[name]
		if !ok || target != filepath.Clean(want) {
			return fmt.Errorf("repair transaction derived-state target is invalid")
		}
	default:
		return fmt.Errorf("repair transaction scope is invalid")
	}
	if change.RemoveOnUndo {
		if change.Scope != "global" || change.PreviousPath != "" {
			return fmt.Errorf("repair transaction remove-on-undo action is invalid")
		}
		return nil
	}
	previous := filepath.Clean(change.PreviousPath)
	if filepath.Dir(previous) == filepath.Dir(target) && strings.HasPrefix(filepath.Base(previous), filepath.Base(target)+".reasonix-") {
		return nil
	}
	if config.MemoryUserDir() == "" {
		return fmt.Errorf("repair transaction state directory is unavailable")
	}
	restoreRoot := filepath.Join(config.MemoryUserDir(), "repair", "restore-backups")
	if repairNodeInsideResolvedRoot(restoreRoot, previous) {
		return nil
	}
	return fmt.Errorf("repair transaction previous path is invalid")
}

// repairNodeInsideResolvedRoot follows directory symlinks but deliberately
// leaves the leaf unresolved. A backed-up config may itself be a symlink whose
// referent is outside the repair directory; cleanup owns that link node, not
// its target. A symlinked parent, however, would let a forged transaction move
// and delete an unrelated node outside the repair root.
func repairNodeInsideResolvedRoot(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || path == "" || root == "." || path == "." {
		return false
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	resolvedPath := filepath.Join(resolvedParent, filepath.Base(path))
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var (
	readRepairPreviousFile = os.ReadFile
	readRepairPreviousLink = os.Readlink
)

// UndoLastRepair restores the exact files moved aside by the latest repair. Any
// currently repaired file is retained as a timestamped redo candidate.
func UndoLastRepair() (*RepairTransaction, error) {
	invocation, err := ReadLastRepair()
	if err != nil {
		return nil, err
	}
	if invocation.Undone {
		return nil, fmt.Errorf("repair %s was already undone", invocation.ID)
	}
	if err := verifyUndoRepairBackups(invocation); err != nil {
		return nil, err
	}
	invocationID := repairPlanStateID(invocation)
	unlockTransaction, err := lockRepairTransaction()
	if err != nil {
		return nil, err
	}
	defer unlockTransaction()
	tx, err := ReadLastRepair()
	if err != nil {
		return nil, err
	}
	if repairPlanStateID(tx) != invocationID {
		return nil, fmt.Errorf("undo repair: last repair transaction changed while waiting")
	}
	if err := verifyUndoRepairBackups(tx); err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(tx.Changes))
	for _, change := range tx.Changes {
		targets = append(targets, change.TargetPath)
	}
	unlockTargets, err := lockRepairMutations(targets...)
	if err != nil {
		return nil, err
	}
	defer unlockTargets()
	current, err := ReadLastRepair()
	if err != nil {
		return nil, fmt.Errorf("undo repair: re-read last repair transaction: %w", err)
	}
	if repairPlanStateID(current) != invocationID {
		return nil, fmt.Errorf("undo repair: last repair transaction changed while waiting")
	}
	tx = current
	if err := verifyUndoRepairBackups(tx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// markUndone persists per-change progress so a failure partway through a
	// multi-change undo leaves a transaction the next undo can resume.
	markUndone := func(i int) error {
		tx.Changes[i].Undone = true
		return persistRepairTransaction(tx)
	}
	for i := len(tx.Changes) - 1; i >= 0; i-- {
		change := tx.Changes[i]
		if change.Undone {
			// Progress was persisted but the backup removal may have been cut
			// short by a crash; finish the cleanup the completed step owed.
			if !change.RemoveOnUndo && change.PreviousPath != "" {
				_ = removeRepairNodeIfMatching(change.PreviousPath, change.TargetPath, change.PreviousStateID)
			}
			continue
		}
		previousStateID := strings.TrimSpace(change.PreviousStateID)
		redo := ""
		// Lstat so a dangling symlink at the target is still moved aside
		// instead of being clobbered by the restore below.
		if _, err := os.Lstat(change.TargetPath); err == nil {
			// Index suffix keeps redo names unique when one undo touches the
			// same target twice (e.g. quarantine + snapshot restore): a shared
			// name would silently overwrite the earlier redo copy.
			redo = fmt.Sprintf("%s.reasonix-redo-%s-%d", change.TargetPath, now.Format("20060102T150405.000000000Z"), i)
			if err := renameRepairNodeNoReplace(change.TargetPath, redo); err != nil {
				return nil, fmt.Errorf("undo repair: retain current file: %w", err)
			}
			repairMutationAfterRename(change.TargetPath)
		}
		if change.RemoveOnUndo {
			if err := markUndone(i); err != nil {
				return nil, err
			}
			continue
		}
		// Restore by copy and keep the backup until the progress record is on
		// disk: consuming the backup first (rename or delete) would leave an
		// unresumable transaction if the process died before markUndone, since
		// the retry's preflight requires the backup of every un-undone change.
		restoreErr := func() error {
			if expected := previousStateID; expected != "" {
				if err := verifyRepairPlanReleaseNodeStateFor(change.PreviousPath, change.TargetPath, expected); err != nil {
					return fmt.Errorf("previous state changed: %w", err)
				}
			}
			info, err := os.Lstat(change.PreviousPath)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				// A quarantined symlink (e.g. a dotfiles-managed config.toml)
				// must come back as a symlink: ReadFile would follow it and
				// materialize a regular file, permanently severing the link.
				// Recreating a symlink is a single atomic syscall, so the
				// crash-safety of the copy path is preserved.
				linkTarget, err := readRepairPreviousLink(change.PreviousPath)
				if err != nil {
					return err
				}
				linkContent, linkContentErr := readRepairPreviousFile(change.PreviousPath)
				exactStateID := repairPlanReadStateIDFor(
					change.TargetPath,
					info.Mode(),
					"symlink",
					linkTarget,
					linkContent,
					linkContentErr == nil,
				)
				if previousStateID != "" && exactStateID != previousStateID {
					return fmt.Errorf("previous link read changed since it was verified")
				}
				if expected := previousStateID; expected != "" {
					if err := verifyRepairPlanReleaseNodeStateFor(change.PreviousPath, change.TargetPath, expected); err != nil {
						return fmt.Errorf("previous state changed while reading link: %w", err)
					}
				}
				return os.Symlink(linkTarget, change.TargetPath)
			}
			b, err := readRepairPreviousFile(change.PreviousPath)
			if err != nil {
				return err
			}
			exactStateID := repairPlanReadStateIDFor(
				change.TargetPath,
				info.Mode(),
				"file",
				"",
				b,
				true,
			)
			if previousStateID != "" && exactStateID != previousStateID {
				return fmt.Errorf("previous file bytes changed since they were verified")
			}
			if expected := previousStateID; expected != "" {
				if err := verifyRepairPlanReleaseNodeStateFor(change.PreviousPath, change.TargetPath, expected); err != nil {
					return fmt.Errorf("previous state changed while reading file: %w", err)
				}
			}
			return fileutil.AtomicCreateFile(change.TargetPath, b, info.Mode().Perm())
		}()
		if restoreErr != nil {
			if redo != "" {
				if compensateErr := restoreRepairNodeIfAbsent(redo, change.TargetPath); compensateErr != nil {
					return nil, fmt.Errorf("undo repair: restore %s: %w (current state retained at %s: %v)", change.TargetPath, restoreErr, redo, compensateErr)
				}
			}
			return nil, fmt.Errorf("undo repair: restore %s: %w", change.TargetPath, restoreErr)
		}
		if err := markUndone(i); err != nil {
			return nil, err
		}
		_ = removeRepairNodeIfMatching(change.PreviousPath, change.TargetPath, previousStateID)
	}
	tx.Undone = true
	tx.UndoneAt = now.Format(time.RFC3339Nano)
	if err := saveRepairTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func verifyUndoRepairBackups(tx *RepairTransaction) error {
	if tx == nil {
		return fmt.Errorf("undo repair: transaction identity is incomplete")
	}
	for _, change := range tx.Changes {
		if change.RemoveOnUndo {
			continue
		}
		expected := strings.TrimSpace(change.PreviousStateID)
		if expected == "" {
			return fmt.Errorf("undo repair: previous state identity is missing; legacy transaction cannot be undone safely")
		}
		if change.Undone {
			continue
		}
		// Lstat: a quarantined symlink counts as present even when its link
		// target is gone. Undo restores the link node, not its referent.
		if _, err := os.Lstat(change.PreviousPath); err != nil {
			return fmt.Errorf("undo repair: previous file %s: %w", change.PreviousPath, err)
		}
		if err := verifyRepairPlanReleaseNodeStateFor(change.PreviousPath, change.TargetPath, expected); err != nil {
			return fmt.Errorf("undo repair: previous state changed: %w", err)
		}
	}
	return nil
}
