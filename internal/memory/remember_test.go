package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRememberToolSaves drives the tool the way the agent does — raw JSON args —
// and verifies the fact lands in the store and the index.
func TestRememberToolSaves(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	tl := NewRememberTool(store)

	if tl.Name() != "remember" || tl.ReadOnly() {
		t.Fatalf("unexpected tool identity: name=%q readonly=%v", tl.Name(), tl.ReadOnly())
	}
	// Schema must be valid JSON the provider can forward.
	if !json.Valid(tl.Schema()) {
		t.Fatal("remember schema is not valid JSON")
	}

	args := []byte(`{"name":"likes-go","title":"Likes Go","description":"User likes Go","type":"user","scope":"global","body":"Default to Go for backend work."}`)
	out, err := tl.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Saved memory") {
		t.Fatalf("unexpected tool output: %q", out)
	}

	list := store.List()
	if len(list) != 1 || list[0].Name != "likes-go" || list[0].Type != TypeUser || list[0].Scope != FactScopeGlobal {
		t.Fatalf("memory not saved correctly: %+v", list)
	}
	if list[0].Title != "Likes Go" {
		t.Fatalf("title not persisted through the tool: %q", list[0].Title)
	}
}

func TestRememberToolDefaultsToProjectScope(t *testing.T) {
	root := t.TempDir()
	store := Store{Dir: root + "/project", GlobalDir: root + "/global"}
	if _, err := NewRememberTool(store).Execute(context.Background(), []byte(`{"name":"project-feedback","description":"current project only","type":"feedback","body":"body"}`)); err != nil {
		t.Fatal(err)
	}
	list := store.List()
	if len(list) != 1 || list[0].Scope != FactScopeProject {
		t.Fatalf("memory = %+v, want project scope", list)
	}
}

func TestRememberToolUpdateWithoutScopePreservesLegacyGlobalScope(t *testing.T) {
	root := t.TempDir()
	store := Store{Dir: filepath.Join(root, "project"), GlobalDir: filepath.Join(root, "global")}
	if err := os.MkdirAll(store.GlobalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "---\nname: no-emoji\ndescription: legacy global feedback\nmetadata:\n  type: feedback\n---\n\nAvoid emoji.\n"
	globalPath := filepath.Join(store.GlobalDir, "no-emoji.md")
	if err := os.WriteFile(globalPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reindexIn(store.GlobalDir, "no-emoji", Memory{Name: "no-emoji", Description: "legacy global feedback", Type: TypeFeedback, Scope: FactScopeGlobal}); err != nil {
		t.Fatal(err)
	}

	out, err := NewRememberTool(store).Execute(context.Background(), []byte(`{"name":"no-emoji","description":"updated global feedback","type":"feedback","body":"Avoid emoji in every project."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(global background)") {
		t.Fatalf("tool output did not report inherited global scope: %q", out)
	}
	if _, err := os.Stat(globalPath); err != nil {
		t.Fatalf("global memory was not kept active: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "no-emoji.md")); !os.IsNotExist(err) {
		t.Fatalf("omitted-scope update created a project copy, stat err=%v", err)
	}
	list := store.List()
	if len(list) != 1 || list[0].Scope != FactScopeGlobal || !strings.Contains(list[0].Body, "every project") {
		t.Fatalf("updated memory = %+v, want one global active copy", list)
	}
}

// TestRememberToolValidates rejects calls missing required fields rather than
// writing an empty memory.
func TestRememberToolValidates(t *testing.T) {
	tl := NewRememberTool(Store{Dir: t.TempDir()})
	if _, err := tl.Execute(context.Background(), []byte(`{"description":"d"}`)); err == nil {
		t.Fatal("expected error when body is missing")
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"body":"b"}`)); err == nil {
		t.Fatal("expected error when description is missing")
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"description":"d","body":"b","scope":"workspace"}`)); err == nil {
		t.Fatal("expected error for an unknown scope")
	}
}

// TestRememberToolQueuesNote verifies a save injects a turn-tail note so the
// fact applies this session, not only the next.
func TestRememberToolQueuesNote(t *testing.T) {
	q := &fakeQueue{}
	ctx := WithQueue(context.Background(), q)
	tl := NewRememberTool(Store{Dir: t.TempDir()})
	if _, err := tl.Execute(ctx, []byte(`{"name":"uses-rmb","description":"balance is RMB","type":"user","body":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if len(q.notes) != 1 || !strings.Contains(q.notes[0], "uses-rmb") || !strings.Contains(q.notes[0], "\nb") {
		t.Fatalf("expected one queued note with the saved memory name and body, got %v", q.notes)
	}
}
