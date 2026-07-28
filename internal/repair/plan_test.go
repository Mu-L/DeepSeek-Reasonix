package repair

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func repairMutationTestKey(path string) string {
	return canonicalRepairPath(path)
}

func TestCanonicalRepairPathUsesFilesystemCaseSemantics(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "Project")
	lower := filepath.Join(root, "project")

	original := repairPathCaseInsensitive
	t.Cleanup(func() { repairPathCaseInsensitive = original })

	repairPathCaseInsensitive = func(string) bool { return false }
	if canonicalRepairPath(upper) == canonicalRepairPath(lower) {
		t.Fatal("case-sensitive filesystem identities were conflated")
	}

	repairPathCaseInsensitive = func(string) bool { return true }
	if canonicalRepairPath(upper) != canonicalRepairPath(lower) {
		t.Fatal("case-insensitive filesystem aliases did not converge")
	}
}

func TestDecodeRepairPlanRejectsUnknownFieldsAndActions(t *testing.T) {
	tests := []string{
		`{"schemaVersion":1,"summary":"x","actions":[{"type":"run_shell","reason":"x"}]}`,
		`{"schemaVersion":1,"summary":"x","actions":[{"type":"rollback_update","reason":"x","command":"rm"}]}`,
		`{"schemaVersion":1,"summary":"x","actions":[{"type":"rebuild_derived_state","target":"sessions","reason":"x"}]}`,
		`{"schemaVersion":1,"summary":"\u001b[2J","actions":[]}`,
	}
	for _, raw := range tests {
		if _, err := DecodeRepairPlan([]byte(raw)); err == nil {
			t.Fatalf("unsafe plan accepted: %s", raw)
		}
	}
}

func TestDecodeRepairPlanAcceptsFencedWhitelistPlan(t *testing.T) {
	raw := "```json\n" + `{"schemaVersion":1,"summary":"repair tabs","actions":[{"type":"rebuild_derived_state","target":"tabs","reason":"malformed"}]}` + "\n```"
	plan, err := DecodeRepairPlan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Target != "tabs" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestDecodeRepairPlanAllowsNoOpPlan(t *testing.T) {
	plan, err := DecodeRepairPlan([]byte(`{"schemaVersion":1,"summary":"no safe repair","actions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("actions = %+v", plan.Actions)
	}
}

func TestRepairPlanIDsBindPlanAndPreviewContent(t *testing.T) {
	plan := RepairPlan{SchemaVersion: 1, Summary: "repair tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview := []RepairPlanPreview{{Index: 1, Type: "rebuild_derived_state", Description: "Quarantine and rebuild derived desktop state: tabs"}}
	if got := RepairPlanID(plan); got != RepairPlanID(plan) || got == "" {
		t.Fatalf("plan ID is not stable: %q", got)
	}
	previewID := RepairPlanPreviewID(plan, preview)
	changedPlan := plan
	changedPlan.Actions = []RepairPlanAction{{Type: "rebuild_derived_state", Target: "window", Reason: "malformed"}}
	if previewID == RepairPlanPreviewID(changedPlan, preview) {
		t.Fatal("changing the action did not change the preview ID")
	}
	changedPreview := append([]RepairPlanPreview(nil), preview...)
	changedPreview[0].Description = "changed preview"
	if previewID == RepairPlanPreviewID(plan, changedPreview) {
		t.Fatal("changing the preview did not change the preview ID")
	}
}

func TestApplyRepairPlanRejectsUnboundPreview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(tabs, []byte("first-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expected := RepairPlanPreviewID(plan, preview)
	if err := os.WriteFile(tabs, []byte("changed-after-preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: expected})
	if err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want stale preview refusal", err)
	}
	if got, readErr := os.ReadFile(tabs); readErr != nil || string(got) != "changed-after-preview" {
		t.Fatalf("stale preview touched derived state: %q, %v", got, readErr)
	}
}

func TestApplyRepairPlanRechecksPreviewBeforeEachAction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(tabs, []byte("bad-tabs"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "rebuild tabs twice", Actions: []RepairPlanAction{
		{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"},
		{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"},
	}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err == nil || !strings.Contains(err.Error(), "action 2: repair plan preview changed") {
		t.Fatalf("error = %v, want second-action stale preview refusal", err)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("applied = %v, want only the confirmed first action", result.Applied)
	}
	if _, statErr := os.Stat(tabs); !os.IsNotExist(statErr) {
		t.Fatalf("second action unexpectedly restored or rewrote tabs: %v", statErr)
	}
}

func TestApplyRepairPlanBindsPendingUpdateTransactionIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "reasonix-desktop")
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return filepath.Join(dir, "reasonix-guard"), nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	tx, err := PrepareFileUpdate("v1", "v2", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "rollback", Actions: []RepairPlanAction{{Type: "rollback_update", Reason: "failed update"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expected := RepairPlanPreviewID(plan, preview)
	tx.CreatedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	if err := WritePendingUpdate(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: expected}); err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want changed transaction refusal", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("stale rollback touched target: %q, %v", got, err)
	}
}

func TestApplyRepairPlanRollsBackCurrentConfirmedUpdateOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "reasonix-desktop")
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return filepath.Join(dir, "reasonix-guard"), nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareFileUpdate("v1", "v2", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "rollback", Actions: []RepairPlanAction{{Type: "rollback_update", Reason: "failed update"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "rolled back update to v1" {
		t.Fatalf("applied = %v, want one successful rollback", result.Applied)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target after rollback = %q, %v", got, err)
	}
	if _, err := os.Stat(PendingUpdatePath()); !os.IsNotExist(err) {
		t.Fatalf("pending update survived successful rollback: %v", err)
	}
}

func TestRollbackPendingUpdateStateBindsCompleteTransaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "reasonix-desktop")
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return filepath.Join(dir, "reasonix-guard"), nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	tx, err := PrepareFileUpdate("v1", "v2", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	expectedState, _ := pendingUpdateBoundPreview(tx)
	tx.FromVersion = "different-old-version"
	if err := WritePendingUpdate(tx); err != nil {
		t.Fatal(err)
	}
	result, err := rollbackPendingUpdateState(expectedState)
	if err != nil {
		t.Fatal(err)
	}
	if result.RolledBack {
		t.Fatalf("changed transaction was rolled back: %+v", result)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("changed transaction touched target: %q, %v", got, err)
	}
	if _, err := ReadPendingUpdate(); err != nil {
		t.Fatalf("changed transaction was consumed: %v", err)
	}
}

func TestApplyRepairPlanRejectsUncooperativeWriteBeforeRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(tabs, []byte("confirmed"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	originalHook := repairMutationBeforeRename
	repairMutationBeforeRename = func(path string) {
		if path == tabs {
			if err := os.WriteFile(path, []byte("changed-in-window"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { repairMutationBeforeRename = originalHook })
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want final state refusal", err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("applied = %v, want no writes", result.Applied)
	}
	if got, err := os.ReadFile(tabs); err != nil || string(got) != "changed-in-window" {
		t.Fatalf("unconfirmed write was quarantined: %q, %v", got, err)
	}
}

func TestApplyRepairPlanPreservesUncooperativeWriteAfterRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(tabs, []byte("confirmed"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	originalHook := repairMutationAfterRename
	repairMutationAfterRename = func(path string) {
		if path == tabs {
			if err := os.WriteFile(path, []byte("new-after-rename"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { repairMutationAfterRename = originalHook })
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err == nil {
		t.Fatal("uncooperative post-rename write was accepted")
	}
	if len(result.Applied) != 0 {
		t.Fatalf("applied = %v, want no successful action", result.Applied)
	}
	if got, err := os.ReadFile(tabs); err != nil || string(got) != "new-after-rename" {
		t.Fatalf("post-rename writer was overwritten: %q, %v", got, err)
	}
	quarantines, err := filepath.Glob(tabs + ".reasonix-rebuild-*")
	if err != nil || len(quarantines) != 1 {
		t.Fatalf("confirmed state backup = %v, %v", quarantines, err)
	}
	if got, err := os.ReadFile(quarantines[0]); err != nil || string(got) != "confirmed" {
		t.Fatalf("confirmed state backup = %q, %v", got, err)
	}
	// The moved node must still be undoable: concurrent rewrite is retained as redo.
	if _, err := UndoLastRepair(); err != nil {
		t.Fatalf("undo after concurrent recreate: %v", err)
	}
	if got, err := os.ReadFile(tabs); err != nil || string(got) != "confirmed" {
		t.Fatalf("undo restored = %q, %v, want confirmed quarantine", got, err)
	}
	redos, err := filepath.Glob(tabs + ".reasonix-redo-*")
	if err != nil || len(redos) != 1 {
		t.Fatalf("redo copies = %v, %v", redos, err)
	}
	if got, err := os.ReadFile(redos[0]); err != nil || string(got) != "new-after-rename" {
		t.Fatalf("redo retained concurrent write = %q, %v", got, err)
	}
}

func TestRepairMutationLockRechecksAfterWaiting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(tabs, []byte("confirmed"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	holder, err := lockRepairMutations(tabs)
	if err != nil {
		t.Fatal(err)
	}
	reachedLock := make(chan struct{})
	originalHook := repairMutationBeforeLock
	repairMutationBeforeLock = func(paths []string) {
		if len(paths) == 1 && paths[0] == repairMutationTestKey(tabs) {
			select {
			case <-reachedLock:
			default:
				close(reachedLock)
			}
		}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalHook })

	resultCh := make(chan struct {
		result ApplyPlanResult
		err    error
	}, 1)
	applyPreviewID := RepairPlanPreviewID(plan, preview)
	go func() {
		result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: applyPreviewID})
		resultCh <- struct {
			result ApplyPlanResult
			err    error
		}{result, err}
	}()
	<-reachedLock
	if err := os.WriteFile(tabs, []byte("changed-while-waiting"), 0o600); err != nil {
		t.Fatal(err)
	}
	holder()
	got := <-resultCh
	if got.err == nil || !strings.Contains(got.err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want post-lock state refusal", got.err)
	}
	if len(got.result.Applied) != 0 {
		t.Fatalf("applied = %v, want no writes", got.result.Applied)
	}
	if data, err := os.ReadFile(tabs); err != nil || string(data) != "changed-while-waiting" {
		t.Fatalf("post-preview state was touched: %q, %v", data, err)
	}
}

func TestRepairTransactionLockSerializesDisjointTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	tabs := filepath.Join(home, "desktop-tabs.json")
	projects := filepath.Join(home, "desktop-projects.json")
	if err := os.WriteFile(tabs, []byte("tabs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projects, []byte("projects"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "tabs", Actions: []RepairPlanAction{{Type: "rebuild_derived_state", Target: "tabs", Reason: "malformed"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tabsPath := repairMutationTestKey(tabs)
	transactionPath := repairMutationTestKey(repairTransactionPath())
	firstHolding := atomic.Bool{}
	tabsReached := make(chan struct{})
	releaseTabs := make(chan struct{})
	secondAttempted := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseTabs:
		default:
			close(releaseTabs)
		}
	})
	originalHook := repairMutationBeforeLock
	repairMutationBeforeLock = func(paths []string) {
		if len(paths) != 1 {
			return
		}
		switch paths[0] {
		case tabsPath:
			firstHolding.Store(true)
			close(tabsReached)
			<-releaseTabs
		case transactionPath:
			if firstHolding.Load() {
				select {
				case <-secondAttempted:
				default:
					close(secondAttempted)
				}
			}
		}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalHook })

	firstResult := make(chan error, 1)
	go func() {
		_, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
		firstResult <- err
	}()
	<-tabsReached
	secondResult := make(chan error, 1)
	go func() {
		_, err := RebuildDerivedState("projects")
		secondResult <- err
	}()
	<-secondAttempted
	select {
	case err := <-secondResult:
		t.Fatalf("disjoint repair bypassed transaction lock: %v", err)
	default:
	}
	if got, err := os.ReadFile(projects); err != nil || string(got) != "projects" {
		t.Fatalf("waiting repair changed project state: %q, %v", got, err)
	}

	close(releaseTabs)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projects); !os.IsNotExist(err) {
		t.Fatalf("serialized repair did not run: %v", err)
	}
}

func TestRepairPlanPreviewDiffMatchesBoundState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	global := filepath.Join(home, "config.toml")
	if err := os.WriteFile(global, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "config", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "global", Reason: "invalid"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(preview[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview[0].Diff, "[broken") || !strings.Contains(string(encoded), preview[0].StateID) {
		t.Fatalf("preview diff/state are not derived from one snapshot: %+v", preview[0])
	}
	if got := repairPlanFileSnapshotAt(global); got.StateID != preview[0].fileStates[global] || string(got.Content) != "[broken\n" {
		t.Fatalf("bound state = %+v, current = %+v", preview[0].fileStates, got)
	}
}

func TestApplyRepairPlanRestoresCurrentConfirmedSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	global := filepath.Join(home, "config.toml")
	if err := os.WriteFile(global, []byte("default_model = \"known-good\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordHealthyConfig("v1"); err != nil {
		t.Fatal(err)
	}
	snapshots, err := ListConfigSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots = %+v, err = %v", snapshots, err)
	}
	current := []byte("default_model = \"current\"\n")
	if err := os.WriteFile(global, current, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "snapshot", Actions: []RepairPlanAction{{Type: "restore_snapshot", SnapshotID: snapshots[0].ID, Reason: "known good"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "restored config snapshot") {
		t.Fatalf("applied = %v", result.Applied)
	}
	if got, err := os.ReadFile(global); err != nil || string(got) != "default_model = \"known-good\"\n" {
		t.Fatalf("restored config = %q, %v", got, err)
	}
	if _, err := UndoLastRepair(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(global); err != nil || string(got) != string(current) {
		t.Fatalf("undo restored = %q, %v", got, err)
	}
}

func TestApplyRepairPlanReportsConfirmedLastKnownGoodRestoreFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	global := filepath.Join(home, "config.toml")
	if err := os.WriteFile(global, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastKnownGood := lastKnownGoodConfigPath()
	if err := os.MkdirAll(filepath.Dir(lastKnownGood), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastKnownGood, []byte("[also-broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := RepairPlan{SchemaVersion: 1, Summary: "config", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "global", Reason: "invalid"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err == nil || !strings.Contains(err.Error(), "restore confirmed last-known-good config") {
		t.Fatalf("error = %v, want confirmed restore failure", err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("applied = %v, want failed action omitted", result.Applied)
	}
	if _, statErr := os.Stat(global); !os.IsNotExist(statErr) {
		t.Fatalf("global config unexpectedly restored: %v", statErr)
	}
	if _, err := UndoLastRepair(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(global); err != nil || string(got) != "[broken\n" {
		t.Fatalf("undo restored = %q, %v", got, err)
	}
}

func TestApplyRepairPlanDoesNotRestoreUnreadableLastKnownGoodNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	global := filepath.Join(home, "config.toml")
	if err := os.WriteFile(global, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastKnownGood := lastKnownGoodConfigPath()
	if err := os.MkdirAll(lastKnownGood, 0o700); err != nil {
		t.Fatal(err)
	}

	plan := RepairPlan{SchemaVersion: 1, Summary: "config", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "global", Reason: "invalid"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: RepairPlanPreviewID(plan, preview)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "quarantined global config") {
		t.Fatalf("applied = %v, want quarantine without restore", result.Applied)
	}
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Fatalf("unreadable source created a global config: %v", err)
	}
}

func TestProjectRepairPlanRequiresExplicitPermission(t *testing.T) {
	plan := RepairPlan{SchemaVersion: 1, Summary: "project", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "project", Reason: "bad toml"}}}
	if _, err := PreviewRepairPlan(plan, ApplyPlanOptions{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "--allow-project") {
		t.Fatalf("preview error = %v", err)
	}
}

func TestApplyRepairPlanMultiActionUndoRevertsWholePlan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	global := filepath.Join(home, "config.toml")
	tabs := filepath.Join(home, "desktop-tabs.json")
	if err := os.WriteFile(global, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tabs, []byte("bad-tabs"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "config + tabs", Actions: []RepairPlanAction{
		{Type: "repair_config", Scope: "global", Reason: "bad toml"},
		{Type: "rebuild_derived_state", Target: "tabs", Reason: "bad tabs"},
	}}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tabs); !os.IsNotExist(err) {
		t.Fatalf("tabs not quarantined: %v", err)
	}
	if _, err := UndoLastRepair(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(global)
	if err != nil || string(got) != "[broken\n" {
		t.Fatalf("global config not restored by plan-level undo: %q, %v", got, err)
	}
	got, err = os.ReadFile(tabs)
	if err != nil || string(got) != "bad-tabs" {
		t.Fatalf("derived state not restored by plan-level undo: %q, %v", got, err)
	}
}

func TestProjectRepairPlanDoesNotRepairGlobalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	root := t.TempDir()
	global := filepath.Join(home, "config.toml")
	project := filepath.Join(root, "reasonix.toml")
	for _, path := range []string{global, project} {
		if err := os.WriteFile(path, []byte("[broken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "project only", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "project", Reason: "bad project toml"}}}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{Root: root, AllowProject: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(global); err != nil {
		t.Fatalf("global config was touched: %v", err)
	}
	if _, err := os.Stat(project); !os.IsNotExist(err) {
		t.Fatalf("project config was not quarantined: %v", err)
	}
}

func TestRepairPlanPreviewIDRejectsSameContentDifferentTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	rootA := t.TempDir()
	rootB := t.TempDir()
	content := []byte("[broken\n")
	for _, root := range []string{rootA, rootB} {
		if err := os.WriteFile(filepath.Join(root, "reasonix.toml"), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "project", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "project", Reason: "bad toml"}}}
	previewA, err := PreviewRepairPlan(plan, ApplyPlanOptions{Root: rootA, AllowProject: true})
	if err != nil {
		t.Fatal(err)
	}
	previewB, err := PreviewRepairPlan(plan, ApplyPlanOptions{Root: rootB, AllowProject: true})
	if err != nil {
		t.Fatal(err)
	}
	idA := RepairPlanPreviewID(plan, previewA)
	idB := RepairPlanPreviewID(plan, previewB)
	if idA == "" || idA == idB {
		t.Fatalf("same-content different roots must not share previewId: a=%s b=%s", idA, idB)
	}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{Root: rootB, AllowProject: true, ExpectedPreviewID: idA}); err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want cross-target preview refusal", err)
	}
	if got, err := os.ReadFile(filepath.Join(rootB, "reasonix.toml")); err != nil || string(got) != string(content) {
		t.Fatalf("project B was modified without confirmation: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "reasonix.toml")); err != nil {
		t.Fatalf("project A was touched: %v", err)
	}
}

func TestRepairMutationLockConvergesSymlinkAliases(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real", "project")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.MkdirAll(linkParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(linkParent, "project")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(realDir, "reasonix.toml")
	aliasFile := filepath.Join(linkDir, "reasonix.toml")
	if err := os.WriteFile(realFile, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if canonicalRepairPath(realFile) != canonicalRepairPath(aliasFile) {
		t.Fatalf("symlink aliases diverged: real=%q alias=%q", canonicalRepairPath(realFile), canonicalRepairPath(aliasFile))
	}

	holder, err := lockRepairMutations(realFile)
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	originalHook := repairMutationBeforeLock
	repairMutationBeforeLock = func(paths []string) {
		if len(paths) == 1 && paths[0] == canonicalRepairPath(aliasFile) {
			select {
			case <-reached:
			default:
				close(reached)
			}
		}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalHook })

	type outcome struct {
		unlock func()
		err    error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		unlock, err := lockRepairMutations(aliasFile)
		resultCh <- outcome{unlock, err}
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		holder()
		t.Fatal("alias lock did not wait on real-path holder")
	}
	select {
	case got := <-resultCh:
		if got.unlock != nil {
			got.unlock()
		}
		holder()
		t.Fatalf("alias lock acquired while real path held: err=%v", got.err)
	case <-time.After(150 * time.Millisecond):
	}
	holder()
	got := <-resultCh
	if got.err != nil {
		t.Fatalf("alias lock after release: %v", got.err)
	}
	got.unlock()
}

func TestApplyRepairPlanRejectsReleaseUnitDriftWithStablePendingUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "reasonix-desktop")
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return filepath.Join(dir, "reasonix-guard"), nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareFileUpdate("v1", "v2", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "rollback", Actions: []RepairPlanAction{{Type: "rollback_update", Reason: "failed update"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expected := RepairPlanPreviewID(plan, preview)
	// Pending transaction stays identical, but the live binary changes again.
	if err := os.WriteFile(target, []byte("newer-unconfirmed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: expected}); err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want release-unit drift refusal", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "newer-unconfirmed" {
		t.Fatalf("stale rollback displaced current binary: %q, %v", got, err)
	}
}

func TestRepairMutationLockSerializesUpdateRollbackAndPrepare(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "reasonix-desktop")
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return filepath.Join(dir, "reasonix-guard"), nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareFileUpdate("v1", "v2", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}

	holder, err := lockRepairMutations(target)
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	originalHook := repairMutationBeforeLock
	repairMutationBeforeLock = func(paths []string) {
		if len(paths) == 1 && paths[0] == canonicalRepairPath(target) {
			select {
			case <-reached:
			default:
				close(reached)
			}
		}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalHook })

	var once sync.Once
	resultCh := make(chan error, 1)
	go func() {
		_, err := RollbackPendingUpdate()
		resultCh <- err
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		holder()
		t.Fatal("rollback did not wait for shared target lock")
	}
	select {
	case err := <-resultCh:
		holder()
		t.Fatalf("rollback completed while target locked: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	once.Do(holder)
	if err := <-resultCh; err != nil {
		t.Fatalf("rollback after target unlock: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target after serialized rollback = %q, %v", got, err)
	}
}

func TestRepairPlanPreviewIDDistinguishesLeafSymlinkTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	base := t.TempDir()
	shared := filepath.Join(base, "shared.toml")
	if err := os.WriteFile(shared, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, filepath.Join(root, "reasonix.toml")); err != nil {
			t.Fatal(err)
		}
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "project", Actions: []RepairPlanAction{{Type: "repair_config", Scope: "project", Reason: "bad toml"}}}
	previewA, err := PreviewRepairPlan(plan, ApplyPlanOptions{Root: rootA, AllowProject: true})
	if err != nil {
		t.Fatal(err)
	}
	previewB, err := PreviewRepairPlan(plan, ApplyPlanOptions{Root: rootB, AllowProject: true})
	if err != nil {
		t.Fatal(err)
	}
	idA := RepairPlanPreviewID(plan, previewA)
	idB := RepairPlanPreviewID(plan, previewB)
	if idA == "" || idA == idB {
		t.Fatalf("leaf symlink targets must not share previewId: a=%s b=%s", idA, idB)
	}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{Root: rootA, AllowProject: true, ExpectedPreviewID: idA}); err != nil {
		t.Fatalf("confirmed leaf symlink repair failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootA, "reasonix.toml")); !os.IsNotExist(err) {
		t.Fatalf("project A symlink was not quarantined: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootB, "reasonix.toml")); err != nil {
		t.Fatalf("project B leaf was touched: %v", err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared referent was removed: %v", err)
	}
}

func TestApplyRepairPlanRejectsAppBundleInteriorDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(base, "Reasonix.app")
	exe := filepath.Join(app, "Contents", "MacOS", "Reasonix")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalExecutable := repairExecutable
	repairExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { repairExecutable = originalExecutable })

	backup := app + ".reasonix-update-backup"
	if err := os.MkdirAll(filepath.Join(backup, "Contents", "MacOS"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "Contents", "MacOS", "Reasonix"), []byte("backup-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareAppBundleUpdate("v1", "v2", app, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := RepairPlan{SchemaVersion: 1, Summary: "rollback", Actions: []RepairPlanAction{{Type: "rollback_update", Reason: "failed update"}}}
	preview, err := PreviewRepairPlan(plan, ApplyPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expected := RepairPlanPreviewID(plan, preview)
	if err := os.WriteFile(exe, []byte("newer-unconfirmed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRepairPlan(plan, ApplyPlanOptions{ExpectedPreviewID: expected}); err == nil || !strings.Contains(err.Error(), "preview changed since confirmation") {
		t.Fatalf("error = %v, want app-bundle interior drift refusal", err)
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "newer-unconfirmed" {
		t.Fatalf("unconfirmed bundle interior was rolled back: %q, %v", got, err)
	}
}
