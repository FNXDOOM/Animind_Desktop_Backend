package downloads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"animind-backend-go/internal/config"
)

// Construction test: the engine builds against the real library with all
// peer-discovery knobs off, and a malformed magnet fails synchronously before
// anything is dialled. Behavioural coverage arrives with Task 11's local seeder.
func TestAnacrolixEngineConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the real library")
	}
	dir := t.TempDir()
	eng, err := NewAnacrolixEngine(config.DownloadsConfig{
		Dir: dir, MaxFiles: 500, MaxBytesPerJob: 25 << 30,
		DownKbps: 0, UpKbps: 500, SeedRatio: 0, SeedMinutes: 0,
		DHT: false, PEX: false, UTP: false, // no network in unit tests
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// An invalid magnet must fail fast, synchronously, without touching the network.
	if _, err := eng.Add(context.Background(), uuid.New(), "not-a-magnet"); err == nil {
		t.Fatal("bad magnet must error")
	}
}

// TestClientConfigDisablesPortForwarding pins the no-port-mapping constraint
// on the client config itself: upstream's torrent.NewClient launches its
// UPnP/NAT-PMP forwardPort goroutine whenever NoDefaultPortForwarding is false
// (the default), so the adapter must always set it. Asserted via the pure
// config builder so the test never binds a port.
func TestClientConfigDisablesPortForwarding(t *testing.T) {
	cc := newClientConfig(config.DownloadsConfig{
		Dir: t.TempDir(), MaxFiles: 500, MaxBytesPerJob: 25 << 30,
		DownKbps: 0, UpKbps: 500, SeedRatio: 0, SeedMinutes: 0,
		DHT: false, PEX: false, UTP: false,
	})
	if !cc.NoDefaultPortForwarding {
		t.Fatal("NoDefaultPortForwarding must be true: the engine must never run UPnP/NAT port mapping")
	}
}

// TestAddParseFailureLeavesNoJobDir pins the symmetric-cleanup contract of Add:
// a magnet that passes the engine's pattern gate but fails upstream parsing
// must not leave an empty placeholder job dir behind.
func TestAddParseFailureLeavesNoJobDir(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the real library")
	}
	dir := t.TempDir()
	eng, err := NewAnacrolixEngine(config.DownloadsConfig{
		Dir: dir, MaxFiles: 500, MaxBytesPerJob: 25 << 30,
		DownKbps: 0, UpKbps: 500, SeedRatio: 0, SeedMinutes: 0,
		DHT: false, PEX: false, UTP: false, // no network in unit tests
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	jobID := uuid.New()
	// 33 infohash chars: accepted by magnetPattern, rejected by upstream's
	// TorrentSpecFromMagnetUri (only 40-char hex / 32-char base32 parse).
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("a", 33)
	if _, err := eng.Add(context.Background(), jobID, magnet); err == nil {
		t.Fatal("magnet that upstream cannot parse must error")
	}
	if _, err := os.Stat(filepath.Join(dir, jobID.String())); !os.IsNotExist(err) {
		t.Fatalf("failed Add must not leave a job dir behind, stat err = %v", err)
	}
}

// TestNoopEngineRejectsAdd pins the disabled-engine contract the manager tests
// rely on: construction always succeeds and every Add is refused.
func TestNoopEngineRejectsAdd(t *testing.T) {
	eng := NewNoopEngine()
	if _, err := eng.Add(context.Background(), uuid.New(),
		"magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("noop Add = %v, want ErrNotEnabled", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("noop Close = %v, want nil", err)
	}
}

// TestMagnetPattern pins the magnet gate: only btih v1 magnets pass, and only
// with a plausible hash body.
func TestMagnetPattern(t *testing.T) {
	valid := []string{
		"magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10",
		"magnet:?xt=urn:btih:S5YXNK7TLDBAQTZDUT2UBIRKQWBQYUBJ&dn=sintel",
		"magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10&dn=x&tr=http%3A%2F%2Ft.example%2Fannounce",
	}
	for _, m := range valid {
		if !magnetPattern.MatchString(m) {
			t.Errorf("magnetPattern rejected %q", m)
		}
	}
	invalid := []string{
		"",
		"not-a-magnet",
		"https://example.com/file.torrent",
		"magnet:?dn=missing-xt",
		"magnet:?xt=urn:btmh:1220caf1e1c30e81cb361b9ee167c4a76c1",
		"magnet:?xt=urn:ed2k:354B15E68FB8F36D7CD88FF94116CDC1",
		"magnet:?xt=urn:btih: spaced hash",
	}
	for _, m := range invalid {
		if magnetPattern.MatchString(m) {
			t.Errorf("magnetPattern accepted %q", m)
		}
	}
}
