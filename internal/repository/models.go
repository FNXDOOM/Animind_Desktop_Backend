package repository

import (
	"errors"
	"time"
)

// ErrNotFound is returned by every repository method that expected exactly one row.
// Handlers map it to 404 (or 401 for credentials), so it must stay distinct from a
// real database failure.
var ErrNotFound = errors.New("repository: row not found")

// User is an account row. IDs are strings rather than uuid types because every
// client speaks them as JSON strings and every query casts them explicitly; this
// keeps the transport, storage and log representations identical.
type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	EmailNormalized string     `json:"-"`
	Username        string     `json:"username"`
	AvatarURL       *string    `json:"avatar_url"`
	PasswordHash    *string    `json:"-"`
	IsAdmin         bool       `json:"is_admin"`
	FailedLogins    int        `json:"-"`
	LockedUntil     *time.Time `json:"-"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// PublicUser is the shape returned to clients: no hash, no lock counters.
type PublicUser struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Username  string    `json:"username"`
	AvatarURL *string   `json:"avatar_url"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Public projects a User onto its client-visible form.
func (u *User) Public() *PublicUser {
	if u == nil {
		return nil
	}
	return &PublicUser{
		ID: u.ID, Email: u.Email, Username: u.Username, AvatarURL: u.AvatarURL,
		IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

// WebSession is a site/browser session. TokenHash is the only stored form of the
// credential; the plaintext token exists solely in the login response.
type WebSession struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	TokenHash  string     `json:"-"`
	DeviceName *string    `json:"device_name"`
	UserAgent  *string    `json:"user_agent"`
	IPAddress  *string    `json:"ip_address"`
	RememberMe bool       `json:"remember_me"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt time.Time  `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// DesktopSession is one desktop device's pinned token pair.
type DesktopSession struct {
	ID                  string     `json:"id"`
	UserID              string     `json:"user_id"`
	SessionID           string     `json:"session_id"`
	DeviceID            *string    `json:"device_id"`
	DeviceName          *string    `json:"device_name"`
	RefreshTokenHash    string     `json:"-"`
	RefreshTokenExpires time.Time  `json:"refresh_token_expires_at"`
	AccessTokenHash     string     `json:"-"`
	AccessTokenExpires  time.Time  `json:"access_token_expires_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// LinkedIdentity maps an external provider account to a user.
type LinkedIdentity struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	Provider       string `json:"provider"`
	ProviderUserID string `json:"provider_user_id"`
	Email          string `json:"email"`
	DisplayName    string `json:"display_name"`
	AvatarURL      string `json:"avatar_url"`
}

// Show is a series row in the indexed library.
//
// Field names match the JSON keys both clients already parse, so no renaming
// happens at the HTTP edge.
type Show struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Synopsis         *string   `json:"synopsis"`
	CoverImageURL    *string   `json:"cover_image_url"`
	Genres           []string  `json:"genres"`
	Rating           *float64  `json:"rating"`
	EpisodeCount     *int      `json:"episode_count"`
	Studio           *string   `json:"studio"`
	Status           *string   `json:"status"`
	Year             *string   `json:"year"`
	AnilistID        *int      `json:"anilist_id"`
	TrailerID        *string   `json:"trailer_id"`
	TrailerSite      *string   `json:"trailer_site"`
	TrailerThumbnail *string   `json:"trailer_thumbnail"`
	CreatedAt        time.Time `json:"created_at"`
}

// Episode is one playable file in the library.
type Episode struct {
	ID                string    `json:"id"`
	ShowID            string    `json:"show_id"`
	SeasonNumber      int       `json:"season_number"`
	ContentType       string    `json:"content_type"`
	EpisodeNumber     *float64  `json:"episode_number"`
	Title             *string   `json:"title"`
	Thumbnail         *string   `json:"thumbnail"`
	Duration          *int      `json:"duration"`
	FilePath          string    `json:"-"`
	BucketName        string    `json:"-"`
	OriginalTitle     *string   `json:"-"`
	ScannerSource     *string   `json:"scanner_source,omitempty"`
	ScannerConfidence *float64  `json:"scanner_confidence,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// ShowWithEpisodes is the GET /api/shows/{id} payload: the show object with its
// episode list attached, matching the flat shape both clients expect.
type ShowWithEpisodes struct {
	Show
	Episodes []Episode `json:"episodes"`
}

// WatchlistItem mirrors the watchlist row shape clients read: anime_data is the
// complete show object the client stored, returned unchanged.
type WatchlistItem struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	AnimeID   string    `json:"anime_id"`
	AnimeData []byte    `json:"anime_data"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProgressItem is a per-episode resume position.
type ProgressItem struct {
	AnimeID      string    `json:"anime_id"`
	EpisodeIndex int       `json:"episode_index"`
	Timestamp    float64   `json:"timestamp"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// HistoryItem is one continue-watching entry written by the desktop player.
type HistoryItem struct {
	UserID       string    `json:"user_id"`
	AnilistID    *string   `json:"anilist_id"`
	ShowTitle    string    `json:"show_title"`
	EpisodeLabel string    `json:"episode_label"`
	ThumbnailURL *string   `json:"thumbnail_url"`
	ProgressPct  int       `json:"progress_pct"`
	LastWatched  time.Time `json:"last_watched"`
}

// PartyRow is the persisted half of a watch room.
type PartyRow struct {
	ID         string     `json:"id"`
	HostUserID string     `json:"host_user_id"`
	HostName   *string    `json:"host_name"`
	EpisodeID  *string    `json:"episode_id"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	EndedAt    *time.Time `json:"ended_at"`
}
