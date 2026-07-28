package repair

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/config"
)

func RebuildDerivedState(target string) ([]string, error) {
	unlockTransaction, err := lockRepairTransaction()
	if err != nil {
		return nil, err
	}
	defer unlockTransaction()
	paths, err := derivedStateTargetPaths(target)
	if err != nil {
		return nil, err
	}
	unlock, err := lockRepairMutations(paths...)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return rebuildDerivedStateUnlocked(target)
}

func rebuildDerivedStateUnlocked(target string) ([]string, error) {
	return rebuildDerivedStateBoundUnlocked(target, nil)
}

func rebuildDerivedStateBoundUnlocked(target string, expectedStates map[string]string) ([]string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	paths := derivedStatePaths()
	var names []string
	if target == "all" {
		for name := range paths {
			names = append(names, name)
		}
		sort.Strings(names)
	} else if _, ok := paths[target]; ok {
		names = []string{target}
	} else {
		return nil, fmt.Errorf("unknown derived-state target %q (want tabs|projects|window|zoom|all)", target)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	applied := []string{}
	tx := newRepairTransaction(time.Now())
	for _, name := range names {
		path := paths[name]
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return applied, err
		}
		if err := verifyRepairPlanFileState(path, expectedStates); err != nil {
			return applied, err
		}
		repairMutationBeforeRename(path)
		if err := verifyRepairPlanFileState(path, expectedStates); err != nil {
			return applied, err
		}
		quarantine := path + ".reasonix-rebuild-" + stamp
		if err := os.Rename(path, quarantine); err != nil {
			return applied, err
		}
		repairMutationAfterRename(path)
		if _, err := os.Lstat(path); err == nil {
			// Confirmed state was already isolated. Persist the move so undo
			// restores it and keeps the concurrent rewrite as a redo copy.
			tx.Changes = append(tx.Changes, RepairChange{Scope: "derived:" + name, TargetPath: path, PreviousPath: quarantine})
			if persistErr := persistRepairTransaction(tx); persistErr != nil {
				return applied, fmt.Errorf("repair plan preview changed since confirmation; target was recreated during quarantine; confirmed state remains at %s; record undo: %v", quarantine, persistErr)
			}
			appendRepairLogBestEffort(tx)
			return applied, fmt.Errorf("repair plan preview changed since confirmation; target was recreated during quarantine; confirmed state remains at %s", quarantine)
		} else if !os.IsNotExist(err) {
			return applied, err
		}
		if expected := expectedStates[path]; expected != "" {
			if err := verifyRepairPlanStateIDFor(quarantine, path, expected); err != nil {
				if restoreErr := restoreRepairNodeIfAbsent(quarantine, path); restoreErr != nil {
					return applied, fmt.Errorf("derived state changed after confirmation and restore failed: %v: %w", restoreErr, err)
				}
				return applied, err
			}
		}
		applied = append(applied, quarantine)
		tx.Changes = append(tx.Changes, RepairChange{Scope: "derived:" + name, TargetPath: path, PreviousPath: quarantine})
		if err := persistRepairTransaction(tx); err != nil {
			_ = os.Rename(quarantine, path)
			return applied, err
		}
	}
	if len(tx.Changes) > 0 {
		appendRepairLogBestEffort(tx)
	}
	return applied, nil
}

func derivedStateTargetPaths(target string) ([]string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	paths := derivedStatePaths()
	if target == "all" {
		names := make([]string, 0, len(paths))
		for name := range paths {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]string, 0, len(names))
		for _, name := range names {
			if path := paths[name]; path != "" {
				out = append(out, path)
			}
		}
		return out, nil
	}
	path, ok := paths[target]
	if !ok {
		return nil, fmt.Errorf("unknown derived-state target %q (want tabs|projects|window|zoom|all)", target)
	}
	if path == "" {
		return []string{}, nil
	}
	return []string{path}, nil
}

func derivedStatePaths() map[string]string {
	paths := map[string]string{}
	if root := config.ReasonixHomeDir(); root != "" {
		paths["tabs"] = filepath.Join(root, "desktop-tabs.json")
		paths["projects"] = filepath.Join(root, "desktop-projects.json")
	}
	if root := config.MemoryUserDir(); root != "" {
		paths["window"] = filepath.Join(root, "desktop-window.json")
		paths["zoom"] = filepath.Join(root, "desktop-zoom.json")
	}
	return paths
}
