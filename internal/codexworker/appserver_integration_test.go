package codexworker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

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
