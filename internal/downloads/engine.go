// Package downloads defines the torrent engine boundary: the manager sees only Engine and
// TorrentHandle, and each torrent stores under <Dir>/<jobID>.
package downloads

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotEnabled is returned by the noop engine, and by the manager when
// downloads are disabled; callers can errors.Is against it.
var ErrNotEnabled = errors.New("downloads: engine disabled")

// ErrMetadataTimeout is returned by Add when the swarm did not deliver the
// metadata before the engine's timeout (or the caller's context) expired.
var ErrMetadataTimeout = errors.New("downloads: metadata timeout")

// EngineFile is one file inside a torrent, in the engine's stable index order.
type EngineFile struct {
	Index     int
	Path      string // relative to the job directory, cleaned
	SizeBytes int64
	Progress  int64
}

// Stats is a point-in-time snapshot of a torrent's transfer state.
type Stats struct {
	BytesDone  int64
	BytesTotal int64
	DownKbps   int64
	UpKbps     int64
	Peers      int
	ETA        time.Duration // 0 when unknown
}

// Engine resolves magnets into torrent handles. The job directory is always
// <root>/<jobID>.
type Engine interface {
	// Add resolves the magnet and returns once metadata is available or the
	// context/timeout expires.
	Add(ctx context.Context, jobID uuid.UUID, magnet string) (TorrentHandle, error)
	Close() error
}

// TorrentHandle drives one torrent the engine has metadata for.
type TorrentHandle interface {
	Files() ([]EngineFile, error)
	Select(indexes []int) error // download only these indexes
	Stats() Stats
	Pause() error
	Resume() error
	Verify(ctx context.Context) error // blocks until checked
	Drop(deleteFiles bool) error
	Done() <-chan struct{} // closed when every selected byte is downloaded
}

// NewNoopEngine returns the disabled engine: every Add is refused with ErrNotEnabled.
// The manager installs it when downloads are disabled and tests use it in place of the library.
func NewNoopEngine() Engine {
	return noopEngine{}
}

type noopEngine struct{}

func (noopEngine) Add(context.Context, uuid.UUID, string) (TorrentHandle, error) {
	return nil, ErrNotEnabled
}

func (noopEngine) Close() error { return nil }
