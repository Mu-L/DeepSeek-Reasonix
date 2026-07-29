package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/repair"
)

func TestLoadWindowsStagedReleaseUnitPreflightsAllMembersAndPublishesDesktopLast(t *testing.T) {
	staging := t.TempDir()
	for name, content := range map[string]string{
		"reasonix-desktop.exe":       "desktop-v2",
		"reasonix-guard.exe":         "guard-v2",
		"reasonix-launcher.exe":      "launcher-v2",
		"reasonix-update-helper.exe": "helper-v2",
		"reasonix-cli.exe":           "cli-v2",
	} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installDir := t.TempDir()
	claimed := &repair.UpdateTransaction{
		SchemaVersion: 1,
		TargetKind:    "file",
		TargetPath:    filepath.Join(installDir, "reasonix-desktop.exe"),
		Files: []repair.UpdateTransactionFile{
			{TargetPath: filepath.Join(installDir, "reasonix-desktop.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-guard.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-launcher.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-update-helper.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-cli.exe")},
			{TargetPath: filepath.Join(installDir, "Reasonix.exe")},
		},
	}

	members, err := loadWindowsStagedReleaseUnit(claimed, staging)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(members[len(members)-1].targetPath); !strings.EqualFold(got, "reasonix-desktop.exe") {
		t.Fatalf("last published member = %q, want desktop", got)
	}
	var published []string
	err = publishLoadedFileUpdateReleaseUnit(claimed, members, func(_ *repair.UpdateTransaction, target string, content []byte, _ os.FileMode) error {
		published = append(published, filepath.Base(target)+"="+string(content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(published, ","); !strings.Contains(got, "Reasonix.exe=launcher-v2") {
		t.Fatalf("portable alias did not reuse launcher payload: %s", got)
	}
	if !strings.HasPrefix(published[len(published)-1], "reasonix-desktop.exe=") {
		t.Fatalf("publish order = %v", published)
	}
}

func TestLoadWindowsStagedReleaseUnitRejectsIncompletePayloadBeforePublish(t *testing.T) {
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "reasonix-desktop.exe"), []byte("desktop-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	claimed := &repair.UpdateTransaction{
		SchemaVersion: 1,
		TargetKind:    "file",
		TargetPath:    filepath.Join(installDir, "reasonix-desktop.exe"),
		Files: []repair.UpdateTransactionFile{
			{TargetPath: filepath.Join(installDir, "reasonix-desktop.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-guard.exe")},
		},
	}
	if _, err := loadWindowsStagedReleaseUnit(claimed, staging); err == nil {
		t.Fatal("incomplete staged release unit was accepted")
	}
}

func TestLoadWindowsStagedReleaseUnitDoesNotCreateMissingPortableAlias(t *testing.T) {
	staging := t.TempDir()
	for name, content := range map[string]string{
		"reasonix-desktop.exe":  "desktop-v2",
		"reasonix-launcher.exe": "launcher-v2",
	} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installDir := t.TempDir()
	claimed := &repair.UpdateTransaction{
		SchemaVersion: 1,
		TargetKind:    "file",
		TargetPath:    filepath.Join(installDir, "reasonix-desktop.exe"),
		Files: []repair.UpdateTransactionFile{
			{TargetPath: filepath.Join(installDir, "reasonix-desktop.exe")},
			{TargetPath: filepath.Join(installDir, "reasonix-launcher.exe")},
			{TargetPath: filepath.Join(installDir, "Reasonix.exe"), MissingBefore: true},
		},
	}

	members, err := loadWindowsStagedReleaseUnit(claimed, staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if strings.EqualFold(filepath.Base(member.targetPath), "Reasonix.exe") {
			t.Fatalf("missing portable alias was added to publish set: %+v", members)
		}
	}
}

func TestPublishLoadedFileUpdateReleaseUnitStopsOnFirstFailedCompareAndPublish(t *testing.T) {
	claimed := &repair.UpdateTransaction{TargetKind: "file"}
	members := []stagedFileUpdateMember{
		{targetPath: "guard.exe", content: []byte("guard"), mode: 0o700},
		{targetPath: "desktop.exe", content: []byte("desktop"), mode: 0o700},
	}
	var published []string
	err := publishLoadedFileUpdateReleaseUnit(claimed, members, func(_ *repair.UpdateTransaction, target string, _ []byte, _ os.FileMode) error {
		published = append(published, target)
		return errors.New("concurrent recreation")
	})
	if err == nil || !strings.Contains(err.Error(), "concurrent recreation") {
		t.Fatalf("publish error = %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published members = %v, want one attempted member", published)
	}
}
