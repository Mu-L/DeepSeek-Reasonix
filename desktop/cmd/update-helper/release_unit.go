package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/repair"
)

type stagedFileUpdateMember struct {
	targetPath string
	content    []byte
	mode       os.FileMode
}

// loadWindowsStagedReleaseUnit validates and reads the complete NSIS payload
// before any live release-unit member is moved. An existing Reasonix.exe is the
// portable alias of reasonix-launcher.exe and reuses those staged bytes; an
// installed package that did not have the alias remains unchanged.
func loadWindowsStagedReleaseUnit(claimed *repair.UpdateTransaction, stagingDir string) ([]stagedFileUpdateMember, error) {
	if claimed == nil || claimed.TargetKind != "file" || len(claimed.Files) == 0 {
		return nil, fmt.Errorf("load staged release unit: transaction identity is incomplete")
	}
	stagingDir = filepath.Clean(strings.TrimSpace(stagingDir))
	if stagingDir == "" || stagingDir == "." || !filepath.IsAbs(stagingDir) {
		return nil, fmt.Errorf("load staged release unit: staging directory is invalid")
	}
	info, err := os.Lstat(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("load staged release unit: inspect staging directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("load staged release unit: staging path is not a directory")
	}

	contents := make(map[string][]byte)
	members := make([]stagedFileUpdateMember, 0, len(claimed.Files))
	seenTargets := make(map[string]struct{}, len(claimed.Files))
	for _, file := range claimed.Files {
		targetPath := filepath.Clean(strings.TrimSpace(file.TargetPath))
		targetKey := strings.ToLower(targetPath)
		if targetPath == "" || targetPath == "." {
			return nil, fmt.Errorf("load staged release unit: target path is invalid")
		}
		if _, ok := seenTargets[targetKey]; ok {
			return nil, fmt.Errorf("load staged release unit: duplicate target %s", filepath.Base(targetPath))
		}
		seenTargets[targetKey] = struct{}{}
		if strings.EqualFold(filepath.Base(targetPath), "Reasonix.exe") && file.MissingBefore {
			continue
		}

		sourceName, err := windowsStagedSourceName(filepath.Base(targetPath))
		if err != nil {
			return nil, err
		}
		sourcePath := filepath.Join(stagingDir, sourceName)
		content, ok := contents[sourceName]
		if !ok {
			sourceInfo, statErr := os.Lstat(sourcePath)
			if statErr != nil {
				return nil, fmt.Errorf("load staged release unit: inspect %s: %w", sourceName, statErr)
			}
			if !sourceInfo.Mode().IsRegular() {
				return nil, fmt.Errorf("load staged release unit: %s is not a regular file", sourceName)
			}
			content, err = os.ReadFile(sourcePath)
			if err != nil {
				return nil, fmt.Errorf("load staged release unit: read %s: %w", sourceName, err)
			}
			contents[sourceName] = content
		}
		members = append(members, stagedFileUpdateMember{
			targetPath: targetPath,
			content:    content,
			mode:       0o700,
		})
	}

	// Publish the running desktop last. If an earlier member fails, the old
	// desktop remains the executable entry point that can report/retry recovery.
	sort.SliceStable(members, func(i, j int) bool {
		iPrimary := strings.EqualFold(members[i].targetPath, claimed.TargetPath)
		jPrimary := strings.EqualFold(members[j].targetPath, claimed.TargetPath)
		return !iPrimary && jPrimary
	})
	return members, nil
}

func windowsStagedSourceName(targetBase string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(targetBase)) {
	case "reasonix-desktop.exe":
		return "reasonix-desktop.exe", nil
	case "reasonix-guard.exe":
		return "reasonix-guard.exe", nil
	case "reasonix-launcher.exe":
		return "reasonix-launcher.exe", nil
	case "reasonix-update-helper.exe":
		return "reasonix-update-helper.exe", nil
	case "reasonix-cli.exe":
		return "reasonix-cli.exe", nil
	case "reasonix.exe":
		return "reasonix-launcher.exe", nil
	default:
		return "", fmt.Errorf("load staged release unit: unsupported target %q", targetBase)
	}
}

func publishLoadedFileUpdateReleaseUnit(
	claimed *repair.UpdateTransaction,
	members []stagedFileUpdateMember,
	publish func(*repair.UpdateTransaction, string, []byte, os.FileMode) error,
) error {
	if publish == nil || len(members) == 0 {
		return fmt.Errorf("publish staged release unit: payload is incomplete")
	}
	for _, member := range members {
		if err := publish(claimed, member.targetPath, member.content, member.mode); err != nil {
			return fmt.Errorf("publish staged release unit %s: %w", filepath.Base(member.targetPath), err)
		}
	}
	return nil
}
