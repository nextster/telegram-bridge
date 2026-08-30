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
}
