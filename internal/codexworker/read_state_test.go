package codexworker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCodexUnreadStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, has_user_event INTEGER NOT NULL, archived INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES('thread-1', 1, 0), ('archived', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	states, err := readCodexUnreadStates(ctx, path)
	if err != nil || !states["thread-1"] {
		t.Fatalf("states=%v err=%v", states, err)
	}
	if _, ok := states["archived"]; ok {
		t.Fatal("archived task was included")
	}
	if err := MarkCodexThreadsRead(ctx, path, []string{"thread-1"}); err != nil {
		t.Fatal(err)
	}
	states, err = readCodexUnreadStates(ctx, path)
	if err != nil || states["thread-1"] {
		t.Fatalf("states after read=%v err=%v", states, err)
	}
}
