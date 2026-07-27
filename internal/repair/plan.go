package repair

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/config"
	textdiff "reasonix/internal/diff"
)

const RepairPlanSchemaVersion = 1

type RepairPlan struct {
	SchemaVersion int                `json:"schemaVersion"`
	Summary       string             `json:"summary"`
	Actions       []RepairPlanAction `json:"actions"`
}

type RepairPlanAction struct {
	Type       string `json:"type"`
	Scope      string `json:"scope,omitempty"`
	SnapshotID string `json:"snapshotId,omitempty"`
	Target     string `json:"target,omitempty"`
	Reason     string `json:"reason"`
}

type RepairPlanPreview struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Diff        string `json:"diff,omitempty"`
	// StateID binds non-display inputs without exposing their paths or content.
	StateID string `json:"stateId,omitempty"`

	rollbackToVersion string
	rollbackCreatedAt string
}

// RepairPlanID identifies the canonical plan content without trusting an ID
// supplied by a caller. It changes when the summary, action list, or any
// action field changes.
func RepairPlanID(plan RepairPlan) string {
	canonical := struct {
		SchemaVersion int                `json:"schemaVersion"`
		Summary       string             `json:"summary"`
		Actions       []RepairPlanAction `json:"actions"`
	}{plan.SchemaVersion, plan.Summary, plan.Actions}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RepairPlanPreviewID binds a plan to the exact preview shown to the user.
// This includes the current filesystem-derived descriptions and diffs, so a
// changed file or action set cannot reuse an earlier confirmation.
func RepairPlanPreviewID(plan RepairPlan, previews []RepairPlanPreview) string {
	canonical := struct {
		PlanID  string              `json:"planId"`
		Preview []RepairPlanPreview `json:"preview"`
	}{RepairPlanID(plan), previews}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func repairPlanStateID(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func repairPlanActionPreviewID(action RepairPlanAction, preview RepairPlanPreview) string {
	preview.Index = 1
	return repairPlanStateID(struct {
		Action  RepairPlanAction  `json:"action"`
		Preview RepairPlanPreview `json:"preview"`
	}{action, preview})
}

func repairPlanFileState(path string) string {
	state := struct {
		Kind       string `json:"kind"`
		Mode       uint32 `json:"mode,omitempty"`
		LinkTarget string `json:"linkTarget,omitempty"`
		Content    string `json:"content,omitempty"`
	}{Kind: "missing"}
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			state.Kind = "unreadable"
		}
		return repairPlanStateID(state)
	}
	state.Kind = "other"
	state.Mode = uint32(info.Mode())
	if info.Mode()&os.ModeSymlink != 0 {
		state.Kind = "symlink"
		state.LinkTarget, _ = os.Readlink(path)
	} else if info.Mode().IsRegular() {
		state.Kind = "file"
	} else if info.IsDir() {
		state.Kind = "directory"
	}
	if b, readErr := os.ReadFile(path); readErr == nil {
		sum := sha256.Sum256(b)
		state.Content = hex.EncodeToString(sum[:])
	} else if state.Kind == "file" || state.Kind == "symlink" {
		state.Kind += "-unreadable"
	}
	return repairPlanStateID(state)
}

func repairPlanDerivedStateID(target string) string {
	paths := derivedStatePaths()
	names := []string{target}
	if target == "all" {
		names = make([]string, 0, len(paths))
		for name := range paths {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	states := make([]struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}, 0, len(names))
	for _, name := range names {
		states = append(states, struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}{Name: name, State: repairPlanFileState(paths[name])})
	}
	return repairPlanStateID(states)
}

type ApplyPlanOptions struct {
	Root         string
	AllowProject bool
	// ExpectedPreviewID binds application to the preview that was confirmed.
	// Empty preserves direct package callers that do not model an approval
	// boundary; CLI confirmation paths always populate it.
	ExpectedPreviewID string
}

type ApplyPlanResult struct {
	Applied []string `json:"applied"`
}

func DecodeRepairPlan(data []byte) (RepairPlan, error) {
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("```")) {
		if start := bytes.IndexByte(data, '{'); start >= 0 {
			if end := bytes.LastIndexByte(data, '}'); end >= start {
				data = data[start : end+1]
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var plan RepairPlan
	if err := dec.Decode(&plan); err != nil {
		return RepairPlan{}, fmt.Errorf("decode repair plan: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return RepairPlan{}, fmt.Errorf("decode repair plan: trailing JSON")
		}
		return RepairPlan{}, fmt.Errorf("decode repair plan: %w", err)
	}
	if err := ValidateRepairPlan(plan); err != nil {
		return RepairPlan{}, err
	}
	return plan, nil
}

func ValidateRepairPlan(plan RepairPlan) error {
	if plan.SchemaVersion != RepairPlanSchemaVersion {
		return fmt.Errorf("repair plan schemaVersion must be %d", RepairPlanSchemaVersion)
	}
	if len(plan.Actions) > 8 {
		return fmt.Errorf("repair plan must contain at most 8 actions")
	}
	if len(plan.Summary) > 1000 {
		return fmt.Errorf("repair plan summary is too long")
	}
	if containsPlanControl(plan.Summary) {
		return fmt.Errorf("repair plan summary contains control characters")
	}
	for i, action := range plan.Actions {
		if len(action.Reason) > 500 {
			return fmt.Errorf("repair action %d reason is too long", i+1)
		}
		if containsPlanControl(action.Reason) {
			return fmt.Errorf("repair action %d reason contains control characters", i+1)
		}
		switch action.Type {
		case "repair_config":
			if action.Scope != "global" && action.Scope != "project" {
				return fmt.Errorf("repair action %d: repair_config scope must be global or project", i+1)
			}
			if action.SnapshotID != "" || action.Target != "" {
				return fmt.Errorf("repair action %d: repair_config has unexpected parameters", i+1)
			}
		case "restore_snapshot":
			if strings.TrimSpace(action.SnapshotID) == "" || action.Scope != "" || action.Target != "" {
				return fmt.Errorf("repair action %d: restore_snapshot requires only snapshotId", i+1)
			}
		case "rebuild_derived_state":
			switch action.Target {
			case "tabs", "projects", "window", "zoom", "all":
			default:
				return fmt.Errorf("repair action %d: invalid derived-state target", i+1)
			}
			if action.Scope != "" || action.SnapshotID != "" {
				return fmt.Errorf("repair action %d: rebuild_derived_state has unexpected parameters", i+1)
			}
		case "rollback_update":
			if action.Scope != "" || action.SnapshotID != "" || action.Target != "" {
				return fmt.Errorf("repair action %d: rollback_update takes no parameters", i+1)
			}
		default:
			return fmt.Errorf("repair action %d: type %q is not allowed", i+1, action.Type)
		}
	}
	return nil
}

func containsPlanControl(text string) bool {
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func PreviewRepairPlan(plan RepairPlan, opts ApplyPlanOptions) ([]RepairPlanPreview, error) {
	if err := ValidateRepairPlan(plan); err != nil {
		return nil, err
	}
	previews := make([]RepairPlanPreview, 0, len(plan.Actions))
	for i, action := range plan.Actions {
		preview := RepairPlanPreview{Index: i + 1, Type: action.Type}
		switch action.Type {
		case "repair_config":
			if action.Scope == "project" && !opts.AllowProject {
				return nil, fmt.Errorf("action %d requires --allow-project", i+1)
			}
			path := config.UserConfigPath()
			if action.Scope == "project" {
				path = projectConfigPath(opts.Root)
			}
			before, _ := os.ReadFile(path)
			after := []byte{}
			afterState := "none"
			if action.Scope == "global" {
				after, _ = os.ReadFile(lastKnownGoodConfigPath())
				afterState = repairPlanFileState(lastKnownGoodConfigPath())
			}
			preview.Description = "Quarantine invalid " + action.Scope + " configuration"
			preview.Diff = textdiff.Build(action.Scope+"-config.toml", string(before), string(after), textdiff.Modify).Diff
			preview.StateID = repairPlanStateID(struct {
				Before string `json:"before"`
				After  string `json:"after"`
			}{repairPlanFileState(path), afterState})
		case "restore_snapshot":
			snap, err := configSnapshotByID(action.SnapshotID)
			if err != nil {
				return nil, err
			}
			if err := verifyConfigSnapshot(snap); err != nil {
				return nil, err
			}
			before, _ := os.ReadFile(config.UserConfigPath())
			after, _ := os.ReadFile(snap.Path)
			preview.Description = "Restore verified global configuration snapshot " + snap.ID
			preview.Diff = textdiff.Build("global-config.toml", string(before), string(after), textdiff.Modify).Diff
			preview.StateID = repairPlanStateID(struct {
				Current  string `json:"current"`
				Snapshot string `json:"snapshot"`
			}{repairPlanFileState(config.UserConfigPath()), repairPlanFileState(snap.Path)})
		case "rebuild_derived_state":
			preview.Description = "Quarantine and rebuild derived desktop state: " + action.Target
			preview.StateID = repairPlanDerivedStateID(action.Target)
		case "rollback_update":
			tx, err := ReadPendingUpdate()
			if err != nil {
				return nil, fmt.Errorf("action %d: no rollback-ready update: %w", i+1, err)
			}
			preview.Description = fmt.Sprintf("Restore Reasonix %s over probationary %s", tx.FromVersion, tx.ToVersion)
			preview.StateID = repairPlanStateID(tx)
			preview.rollbackToVersion = tx.ToVersion
			preview.rollbackCreatedAt = tx.CreatedAt
		}
		previews = append(previews, preview)
	}
	return previews, nil
}

func ApplyRepairPlan(plan RepairPlan, opts ApplyPlanOptions) (ApplyPlanResult, error) {
	preview, err := PreviewRepairPlan(plan, opts)
	if err != nil {
		return ApplyPlanResult{Applied: []string{}}, err
	}
	expected := strings.TrimSpace(opts.ExpectedPreviewID)
	if expected != "" {
		actual := RepairPlanPreviewID(plan, preview)
		if expected != actual {
			return ApplyPlanResult{Applied: []string{}}, fmt.Errorf("repair plan preview changed since confirmation; re-preview and re-confirm (expected %s, got %s)", expected, actual)
		}
	}
	boundPreview := preview
	result := ApplyPlanResult{Applied: []string{}}
	// Each action records its own transaction in last-repair.json, so a
	// multi-action plan would otherwise leave only its final action undoable.
	// Merge every transaction the plan produces into one plan-level
	// transaction, persisted after each action so a mid-plan failure still
	// leaves the already-applied prefix fully undoable.
	planTx := newRepairTransaction(time.Now())
	absorbed := 0
	lastSeenID := ""
	if last, err := ReadLastRepair(); err == nil {
		lastSeenID = last.ID
	}
	absorbRepair := func() error {
		last, err := ReadLastRepair()
		if err != nil || last.ID == lastSeenID {
			return nil
		}
		lastSeenID = last.ID
		planTx.Changes = append(planTx.Changes, last.Changes...)
		absorbed++
		if absorbed < 2 {
			// A single recorded action is already exactly the last repair.
			return nil
		}
		return persistRepairTransaction(planTx)
	}
	for i, action := range plan.Actions {
		if expected != "" {
			current, previewErr := PreviewRepairPlan(RepairPlan{
				SchemaVersion: plan.SchemaVersion,
				Summary:       plan.Summary,
				Actions:       []RepairPlanAction{action},
			}, opts)
			if previewErr != nil {
				return result, fmt.Errorf("action %d: repair plan preview changed since confirmation; re-preview and re-confirm: %w", i+1, previewErr)
			}
			expectedAction := repairPlanActionPreviewID(action, boundPreview[i])
			actualAction := repairPlanActionPreviewID(action, current[0])
			if expectedAction != actualAction {
				return result, fmt.Errorf("action %d: repair plan preview changed since confirmation; re-preview and re-confirm (expected %s, got %s)", i+1, expectedAction, actualAction)
			}
			boundPreview[i] = current[0]
		}
		var actionErr error
		switch action.Type {
		case "repair_config":
			report, err := InspectAndRepairConfig(ConfigOptions{Root: opts.Root, Apply: true, IncludeProject: action.Scope == "project", OnlyScope: action.Scope})
			if err != nil {
				actionErr = err
			} else {
				result.Applied = append(result.Applied, report.Applied...)
			}
		case "restore_snapshot":
			tx, err := RestoreConfigSnapshot(action.SnapshotID)
			if err != nil {
				actionErr = err
			} else {
				result.Applied = append(result.Applied, "restored config snapshot (undo "+tx.ID+")")
			}
		case "rebuild_derived_state":
			paths, err := RebuildDerivedState(action.Target)
			if err != nil {
				actionErr = err
			} else {
				result.Applied = append(result.Applied, paths...)
			}
		case "rollback_update":
			rollback, err := RollbackPendingUpdate()
			if expected != "" {
				rollback, err = rollbackPendingUpdate(boundPreview[i].rollbackToVersion, boundPreview[i].rollbackCreatedAt)
			}
			if err != nil {
				actionErr = err
			} else if rollback.RolledBack {
				result.Applied = append(result.Applied, "rolled back update to "+rollback.ToVersion)
			} else if expected != "" {
				actionErr = fmt.Errorf("repair plan preview changed since confirmation; re-preview and re-confirm")
			}
		}
		// Absorb even on failure: a partially applied action may have recorded
		// changes that the plan-level undo must cover.
		if mergeErr := absorbRepair(); mergeErr != nil && actionErr == nil {
			return result, fmt.Errorf("action %d: record plan transaction: %w", i+1, mergeErr)
		}
		if actionErr != nil {
			return result, fmt.Errorf("action %d: %w", i+1, actionErr)
		}
	}
	return result, nil
}

func configSnapshotByID(id string) (ConfigSnapshot, error) {
	snapshots, err := ListConfigSnapshots()
	if err != nil {
		return ConfigSnapshot{}, err
	}
	for _, snap := range snapshots {
		if snap.ID == id {
			return snap, nil
		}
	}
	return ConfigSnapshot{}, fmt.Errorf("config snapshot %q not found", id)
}

func projectConfigPath(root string) string {
	root = strings.TrimSpace(root)
	if root == "" || root == "." {
		return "reasonix.toml"
	}
	return filepath.Join(root, "reasonix.toml")
}
