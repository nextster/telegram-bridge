package codexworker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLastVisibleMessage(t *testing.T) {
	user, _ := json.Marshal(map[string]any{
		"type":    "userMessage",
		"content": []map[string]string{{"type": "text", "text": "question"}, {"type": "image"}},
	})
	assistant, _ := json.Marshal(map[string]string{"type": "agentMessage", "text": "**answer**"})
	thread := appServerThread{}
	thread.Turns = append(thread.Turns, struct {
		Status string            `json:"status"`
		Items  []json.RawMessage `json:"items"`
	}{Status: "completed", Items: []json.RawMessage{user, assistant}})
	role, message := lastVisibleMessage(thread)
	if role != "assistant" || message != "**answer**" {
		t.Fatalf("lastVisibleMessage() = %q, %q", role, message)
	}
}

func TestAppServerIntegration(t *testing.T) {
	if os.Getenv("CODEX_APP_SERVER_INTEGRATION") != "1" {
		t.Skip("set CODEX_APP_SERVER_INTEGRATION=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := RunAppServer(ctx, AppServerConfig{
		CodexBin:       "codex",
		CWD:            t.TempDir(),
		Prompt:         "Reply with exactly: bridge-pong",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
		Ephemeral:      true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID == "" || !strings.Contains(result.Text, "bridge-pong") {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := ListArchivedThreadIDs(ctx, "codex"); err != nil {
		t.Fatalf("list archived threads: %v", err)
	}
	snapshots, state, err := ListActiveThreadSnapshots(ctx, "codex", nil)
	if err != nil {
		t.Fatalf("list active threads: %v", err)
	}
	if len(state) == 0 || len(snapshots) == 0 {
		t.Fatalf("active thread sync returned snapshots=%d state=%d", len(snapshots), len(state))
	}
	statuses := map[string]int{}
	for _, snapshot := range snapshots {
		statuses[snapshot.Status]++
	}
	t.Logf("active task sync loaded %d snapshots from %d top-level tasks; statuses=%v", len(snapshots), len(state), statuses)
}

func TestAppServerForksWhenThreadHasActiveWriter(t *testing.T) {
	threadID := os.Getenv("CODEX_ACTIVE_WRITER_THREAD_ID")
	if threadID == "" {
		t.Skip("set CODEX_ACTIVE_WRITER_THREAD_ID to an open Codex task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := RunAppServer(ctx, AppServerConfig{
		CodexBin:       "codex",
		CWD:            "/path/to/telegram-bridge",
		ThreadID:       threadID,
		Prompt:         "Reply with exactly: fork-pong",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID == threadID {
		t.Fatalf("thread was not forked: %s", result.ThreadID)
	}
	if !strings.Contains(result.Text, "fork-pong") {
		t.Fatalf("unexpected result: %#v", result)
	}
}
