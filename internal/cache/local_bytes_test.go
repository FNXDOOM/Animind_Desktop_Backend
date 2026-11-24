package cache

import (
	"context"
	"testing"
	"time"
)

func TestLocalStoreEnforcesByteBudget(t *testing.T) {
	store := newLocal()
	store.maxBytes = 100
	ctx := context.Background()
	value := make([]byte, 60)

	for i := 0; i < 3; i++ {
		if err := store.Set(ctx, string(rune('a'+i)), value, time.Hour); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	store.mu.Lock()
	bytes := store.bytes
	entries := len(store.entries)
	store.mu.Unlock()

	if bytes > store.maxBytes {
		t.Fatalf("cache bytes = %d, budget = %d", bytes, store.maxBytes)
	}
	if entries > 2 {
		t.Fatalf("cache retained %d entries, want at most 2", entries)
	}
}

func TestLocalStoreRefusesOversizeDocument(t *testing.T) {
	store := newLocal()
	store.maxBytes = 10
	if err := store.Set(context.Background(), "big", make([]byte, 11), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, found, _ := store.Get(context.Background(), "big"); found {
		t.Fatal("a value larger than the whole budget was cached")
	}
}

func TestLocalStoreDeleteReclaimsBytes(t *testing.T) {
	store := newLocal()
	value := make([]byte, 32)
	if err := store.Set(context.Background(), "k", value, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.bytes != 0 {
		t.Fatalf("bytes after delete = %d, want 0", store.bytes)
	}
}
