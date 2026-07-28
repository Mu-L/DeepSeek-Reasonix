package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

type ConfigCheck struct {
	Scope        string `json:"scope"`
	Path         string `json:"path"`
	Exists       bool   `json:"exists"`
	Valid        bool   `json:"valid"`
	Error        string `json:"error,omitempty"`
	SnapshotPath string `json:"snapshotPath,omitempty"`
}

type ConfigReport struct {
	Checks  []ConfigCheck `json:"checks"`
	Applied []string      `json:"applied"`
}

type ConfigOptions struct {
	Root           string
	Apply          bool
	IncludeProject bool
	OnlyScope      string
	Now            func() time.Time

	expectedStates         map[string]string
	confirmedGlobalRestore []byte
	hasConfirmedRestore    bool
}

func InspectAndRepairConfig(opts ConfigOptions) (ConfigReport, error) {
	if !opts.Apply {
		return inspectAndRepairConfigUnlocked(opts)
	}
	unlockTransaction, err := lockRepairTransaction()
	if err != nil {
		return ConfigReport{}, err
	}
	defer unlockTransaction()
	paths, err := configRepairTargetPaths(opts)
	if err != nil {
		return ConfigReport{}, err
	}
	unlock, err := lockRepairMutations(paths...)
	if err != nil {
		return ConfigReport{}, err
	}
	defer unlock()
	return inspectAndRepairConfigUnlocked(opts)
}

func inspectAndRepairConfigUnlocked(opts ConfigOptions) (ConfigReport, error) {
	if opts.OnlyScope != "" && opts.OnlyScope != "global" && opts.OnlyScope != "project" {
		return ConfigReport{}, fmt.Errorf("unknown config repair scope %q", opts.OnlyScope)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	global := config.UserConfigPath()
	project := filepath.Join(opts.Root, "reasonix.toml")
	if opts.Root == "" || opts.Root == "." {
		project = "reasonix.toml"
	}
	paths := []struct{ scope, path string }{{"global", global}, {"project", project}}
	report := ConfigReport{Checks: make([]ConfigCheck, 0, len(paths)), Applied: []string{}}
	tx := newRepairTransaction(opts.Now())
	for _, item := range paths {
		check := inspectConfig(item.scope, item.path)
		if item.scope == "global" {
			check.SnapshotPath = lastKnownGoodConfigPath()
		}
		report.Checks = append(report.Checks, check)
		if !opts.Apply || !check.Exists || check.Valid || (opts.OnlyScope != "" && item.scope != opts.OnlyScope) || (item.scope == "project" && !opts.IncludeProject) {
			continue
		}
		if err := verifyRepairPlanFileState(item.path, opts.expectedStates); err != nil {
			return report, err
		}
		if item.scope == "global" {
			if err := verifyRepairPlanFileState(lastKnownGoodConfigPath(), opts.expectedStates); err != nil {
				return report, err
			}
		}
		repairMutationBeforeRename(item.path)
		if err := verifyRepairPlanFileState(item.path, opts.expectedStates); err != nil {
			return report, err
		}
		if item.scope == "global" {
			if err := verifyRepairPlanFileState(lastKnownGoodConfigPath(), opts.expectedStates); err != nil {
				return report, err
			}
		}
		quarantine := item.path + ".reasonix-quarantine-" + opts.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(item.path, quarantine); err != nil {
			return report, fmt.Errorf("quarantine %s config: %w", item.scope, err)
		}
		repairMutationAfterRename(item.path)
		if _, err := os.Lstat(item.path); err == nil {
			// The confirmed node already moved; record it so UndoLastRepair can
			// restore it and retain the concurrent rewrite as a redo copy.
			tx.Changes = append(tx.Changes, RepairChange{TargetPath: item.path, PreviousPath: quarantine, Scope: item.scope})
			if persistErr := persistRepairTransaction(tx); persistErr != nil {
				return report, fmt.Errorf("repair plan preview changed since confirmation; target was recreated during quarantine; confirmed state remains at %s; record undo: %v", quarantine, persistErr)
			}
			appendRepairLogBestEffort(tx)
			return report, fmt.Errorf("repair plan preview changed since confirmation; target was recreated during quarantine; confirmed state remains at %s", quarantine)
		} else if !os.IsNotExist(err) {
			return report, err
		}
		if expected := opts.expectedStates[item.path]; expected != "" {
			if err := verifyRepairPlanStateIDFor(quarantine, item.path, expected); err != nil {
				if restoreErr := restoreRepairNodeIfAbsent(quarantine, item.path); restoreErr != nil {
					return report, fmt.Errorf("quarantine %s config changed after confirmation and restore failed: %v: %w", item.scope, restoreErr, err)
				}
				return report, err
			}
		}
		report.Applied = append(report.Applied, "quarantined "+item.scope+" config at "+quarantine)
		tx.Changes = append(tx.Changes, RepairChange{TargetPath: item.path, PreviousPath: quarantine, Scope: item.scope})
		if err := persistRepairTransaction(tx); err != nil {
			_ = os.Rename(quarantine, item.path)
			return report, err
		}
		if item.scope == "global" {
			restoreErr := os.ErrNotExist
			if opts.expectedStates != nil {
				if opts.hasConfirmedRestore {
					if restoreErr = config.ValidateBytes(opts.confirmedGlobalRestore); restoreErr == nil {
						restoreErr = fileutil.AtomicCreateFile(item.path, opts.confirmedGlobalRestore, 0o600)
					}
				}
			} else {
				restoreErr = restoreLastKnownGoodConfig(item.path)
			}
			if restoreErr == nil {
				report.Applied = append(report.Applied, "restored global config from last-known-good snapshot")
			} else if opts.expectedStates != nil && opts.hasConfirmedRestore {
				return report, fmt.Errorf("restore confirmed last-known-good config: %w", restoreErr)
			}
		}
		report.Checks[len(report.Checks)-1] = inspectConfig(item.scope, item.path)
		if item.scope == "global" {
			report.Checks[len(report.Checks)-1].SnapshotPath = lastKnownGoodConfigPath()
		}
	}
	if len(tx.Changes) > 0 {
		appendRepairLogBestEffort(tx)
	}
	return report, nil
}

func configRepairTargetPaths(opts ConfigOptions) ([]string, error) {
	if opts.OnlyScope != "" && opts.OnlyScope != "global" && opts.OnlyScope != "project" {
		return nil, fmt.Errorf("unknown config repair scope %q", opts.OnlyScope)
	}
	project := filepath.Join(opts.Root, "reasonix.toml")
	if opts.Root == "" || opts.Root == "." {
		project = "reasonix.toml"
	}
	switch opts.OnlyScope {
	case "global":
		return []string{config.UserConfigPath()}, nil
	case "project":
		return []string{project}, nil
	default:
		paths := []string{config.UserConfigPath()}
		if opts.IncludeProject {
			paths = append(paths, project)
		}
		return paths, nil
	}
}

func inspectConfig(scope, path string) ConfigCheck {
	check := ConfigCheck{Scope: scope, Path: path, Valid: true}
	if path == "" {
		return check
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			check.Valid = false
			check.Error = err.Error()
		}
		return check
	}
	check.Exists = true
	if err := config.ValidateFile(path); err != nil {
		check.Valid = false
		check.Error = err.Error()
	}
	return check
}

type snapshotMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	SourcePath    string `json:"sourcePath"`
	RecordedAt    string `json:"recordedAt"`
	Version       string `json:"version,omitempty"`
}

func RecordHealthyConfig(version string) error {
	path := config.UserConfigPath()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := config.ValidateFile(path); err != nil {
		return err
	}
	snapshot := lastKnownGoodConfigPath()
	if snapshot == "" {
		return nil
	}
	if err := fileutil.AtomicWriteFile(snapshot, b, 0o600); err != nil {
		return err
	}
	now := time.Now().UTC()
	meta := snapshotMeta{SchemaVersion: 1, SourcePath: path, RecordedAt: now.Format(time.RFC3339Nano), Version: version}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(snapshot+".json", append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return recordConfigSnapshot(path, b, version, now)
}

func lastKnownGoodConfigPath() string {
	root := config.MemoryUserDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "repair", "config.toml.last-known-good")
}

func restoreLastKnownGoodConfig(dest string) error {
	snapshot := lastKnownGoodConfigPath()
	if err := config.ValidateFile(snapshot); err != nil {
		return err
	}
	b, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(dest, b, 0o600)
}
