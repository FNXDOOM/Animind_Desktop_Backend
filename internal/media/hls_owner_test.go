package media

import (
	"context"
	"strings"
	"testing"
)

func TestHLSManagerBindsControlToOwner(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("a", 32)
	manager := &HLSManager{sessions: map[string]*managedSession{
		id: {session: &HLSSession{ID: id, UserID: "user-a", Dir: dir}},
	}}

	if owner, ok := manager.Owner(id); !ok || owner != "user-a" {
		t.Fatalf("Owner = %q, %v; want user-a, true", owner, ok)
	}
	if manager.DestroyForUser(context.Background(), id, "user-b") {
		t.Fatal("a different user destroyed the session")
	}
	if _, ok := manager.Owner(id); !ok {
		t.Fatal("the session was removed by a non-owner")
	}
	if !manager.DestroyForUser(context.Background(), id, "user-a") {
		t.Fatal("the owner could not destroy the session")
	}
	if _, ok := manager.Owner(id); ok {
		t.Fatal("the session survived an owner destroy")
	}
}

func TestHLSManagerSeekRejectsNonOwner(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("b", 32)
	manager := &HLSManager{sessions: map[string]*managedSession{
		id: {session: &HLSSession{ID: id, UserID: "user-a", Dir: dir}},
	}}

	if err := manager.Seek(context.Background(), id, 5, "user-b"); err != ErrUnknownSession {
		t.Fatalf("Seek by non-owner = %v, want ErrUnknownSession", err)
	}
}
