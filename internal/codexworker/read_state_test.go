package codexworker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexUnreadStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex-global-state.json")
	state := map[string]any{
		"unrelated": map[string]any{"preserved": true},
		"electron-persisted-atom-state": map[string]any{
			codexUnreadAtomKey: map[string][]string{
				"local":  {"thread-1", "thread-2"},
				"remote": {"remote-thread"},
			},
		},
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}

	states, err := readCodexUnreadStates(path)
	if err != nil || !states["thread-1"] {
		t.Fatalf("states=%v err=%v", states, err)
	}
	if err := MarkCodexThreadsRead(path, []string{"thread-1"}); err != nil {
		t.Fatal(err)
	}
	states, err = readCodexUnreadStates(path)
	if err != nil || states["thread-1"] {
		t.Fatalf("states after read=%v err=%v", states, err)
	}
	if !states["thread-2"] {
		t.Fatal("unrelated unread task was removed")
	}
	root, atoms, err := readCodexGlobalState(path)
	if err != nil || len(root["unrelated"]) == 0 {
		t.Fatalf("unrelated root state was not preserved: err=%v", err)
	}
	var byHost map[string][]string
	if err := json.Unmarshal(atoms[codexUnreadAtomKey], &byHost); err != nil {
		t.Fatal(err)
	}
	if len(byHost["remote"]) != 1 || byHost["remote"][0] != "remote-thread" {
		t.Fatalf("remote state changed: %v", byHost)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}
