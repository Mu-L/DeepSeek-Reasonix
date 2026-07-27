package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
