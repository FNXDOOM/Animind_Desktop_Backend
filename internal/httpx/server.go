package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/downloads"
	"animind-backend-go/internal/media"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
	"animind-backend-go/internal/storage"
	"animind-backend-go/internal/syncplay"
	"animind-backend-go/internal/torrent"
)

// Server is the assembled HTTP API: routes, middleware and every dependency a handler
// needs. Handlers are methods on it, so a route table reads as a list of endpoints rather
// than a list of constructors.

// SessionCookieName is the cookie a browser session is stored in.
const SessionCookieName = "animind_session"

// maxJSONBody bounds one request body.
const maxJSONBody = int64(1 << 20)

// Deps are the collaborators the API needs. Every one is required except the optional
// local-mode services, which answer 400 when their mode is not active, and Cache, whose
// absence keeps credential and rate-limit state inside this process. Torrent, Downloads,
// Trending and EventHub stay nil until the deployment enables them (local storage mode).
type Deps struct {
	Config  *config.Config
	Store   *repository.Store
	Auth    *auth.Service
	Signer  *auth.Signer
	Cache   cache.Store
	Storage *storage.Service
	Media   *media.FFmpeg
	Audio   *media.AudioCache
	HLS     *media.HLSManager
	Scanner *scanner.Service
	Hub     *syncplay.Hub
	Logger  *slog.Logger

	// Torrent serves /api/torrents/search; nil (or an empty TORZNAB_URL) answers 501.
	// The narrow interface is what the handlers consume; *torrent.Service is the
	// production implementation.
	Torrent torrentServiceAPI
	// Downloads drives the torrent job state machine; the handlers answer 501 when
	// downloads are disabled.
	Downloads *downloads.Manager
	// Trending is the same counter instance the Torrent service records into — a
	// second instance would silently fork the counts.
	Trending *torrent.Trending
	// EventHub is the same SSE hub the Downloads manager publishes into.
	EventHub *downloads.Hub
}

// Server hosts the API.
type Server struct {
	cfg       *config.Config
	store     *repository.Store
	auth      *auth.Service
	signer    *auth.Signer
	cache     cache.Store
	storage   *storage.Service
	media     *media.FFmpeg
	audio     *media.AudioCache
	hls       *media.HLSManager
	scanner   *scanner.Service
	hub       *syncplay.Hub
	limiter   *RateLimitPresets
	logger    *slog.Logger
	startedAt time.Time

	// The download surface sits behind narrow interfaces so handler tests can
	// fake the collaborators without a database. *torrent.Service and
	// *downloads.Manager satisfy them as-is.
	torrent      torrentServiceAPI
	downloads    downloadsManagerAPI
	eventsHub    *downloads.Hub
	trending     *torrent.Trending
	jobStore     downloadsJobStore
	entitlements downloadEntitlement

	// aniListHTTP is the client used by the GraphQL proxy, with a timeout long enough for a
	// complex search but short enough to release a worker.
	aniListHTTP *http.Client

	// metrics collects the counters the Prometheus listener exports. Nil when metrics are
	// disabled, and every record call is a no-op then.
	metrics *metrics

	httpServer *http.Server

	// metricsServer is the scrape listener. It is separate because it is unauthenticated:
	// nothing may reach it from a public interface.
	metricsServer *http.Server
}

// New builds the server and its per-route rate limiters.
func New(deps Deps) *Server {
	s := &Server{
		cfg:         deps.Config,
		store:       deps.Store,
		auth:        deps.Auth,
		signer:      deps.Signer,
		cache:       deps.Cache,
		storage:     deps.Storage,
		media:       deps.Media,
		audio:       deps.Audio,
		hls:         deps.HLS,
		scanner:     deps.Scanner,
		hub:         deps.Hub,
		limiter:     NewRateLimitPresets(deps.Cache, deps.Logger),
		logger:      deps.Logger,
		startedAt:   time.Now(),
		aniListHTTP: &http.Client{Timeout: 15 * time.Second},
		torrent:     deps.Torrent,
		downloads:   deps.Downloads,
		eventsHub:   deps.EventHub,
		trending:    deps.Trending,
	}
	if deps.Store != nil {
		s.jobStore = deps.Store
		s.entitlements = storeEntitlements{store: deps.Store}
	}
	if deps.Config.Metrics.Enabled {
		// The download manager joins the same registry: newMetrics registers the
		// downloads collectors and hands itself over as the manager's observer.
		s.metrics = newMetrics(deps.Cache, deps.Hub, deps.Store, s.limiter.fallback, deps.Downloads)
	}
	return s
}

// credential extracts the bearer token, falling back to the session cookie.
//
// The site may authenticate either way: a bearer header for API calls made from JS, and an
// HttpOnly cookie where the SPA must not hold the token at all. The desktop player only ever
// sends the header.
func credential(r *http.Request) string {
	if token := BearerToken(r); token != "" {
		return token
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

// rate returns a per-route limiter middleware keyed by client address.
func (s *Server) rate(limiter *RateLimiter) func(http.Handler) http.Handler {
	return limiter.MiddlewareByIP(s.cfg.TrustProxy)
}

// Router builds the chi route table.
//
// Route paths and JSON shapes are unchanged from the Node backend wherever a shipped client
// parses them, so the existing desktop player and site work against this API without an
// update. The added /api/auth/* and /api/me/* routes are what replace Clerk and the browser's
// direct database access.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()

	r.Use(Recover)
	r.Use(RequestLogger)
	r.Use(s.observeRequests)
	r.Use(SecureHeaders)
	r.Use(s.CORS)
	r.Use(chimiddleware.StripSlashes)

	// Health and diagnostics.
	r.Get("/health", s.handleHealth)
	r.Get("/api/health", s.handleHealth)

	// Catalog, public by design: both clients browse the library before signing in.
	r.Route("/api", func(api chi.Router) {
		api.With(s.rate(s.limiter.PublicRead)).Get("/shows", s.handleListShows)
		api.With(s.rate(s.limiter.PublicRead)).Get("/shows/{id}", s.handleShowByID)
		api.With(s.rate(s.limiter.PublicRead)).Post("/anilist", s.handleAniListProxy)

		// Storage webhook: guarded by a shared secret header, not by a user session.
		api.With(s.rate(s.limiter.Webhook)).Post("/webhooks/storage", s.handleStorageWebhook)

		// Authentication.
		api.Route("/auth", func(authz chi.Router) {
			authz.With(s.rate(s.limiter.Signup)).Post("/signup", s.handleSignup)
			authz.With(s.rate(s.limiter.Login)).Post("/login", s.handleLogin)
			authz.With(s.rate(s.limiter.Login)).Post("/logout", s.handleLogout)
			authz.With(s.rate(s.limiter.Login)).Get("/google/url", s.handleGoogleURL)
			authz.With(s.rate(s.limiter.Login)).Get("/google/start", s.handleGoogleStart)
			authz.With(s.rate(s.limiter.Login)).Get("/google/callback", s.handleGoogleCallback)
			authz.With(s.rate(s.limiter.Login)).Post("/consume-code", s.handleConsumeCode)
			authz.With(s.rate(s.limiter.Login)).Post("/password/forgot", s.handleForgotPassword)
			authz.With(s.rate(s.limiter.Login)).Post("/password/reset", s.handleResetPassword)

			// Desktop device pairing and renewal. exchange requires a signed-in web
			// credential; refresh and revoke are keyed by their own tokens.
			authz.With(s.rate(s.limiter.Login)).Post("/desktop/exchange", s.handleDesktopExchange)
			authz.With(s.rate(s.limiter.Login)).Post("/desktop/refresh", s.handleDesktopRefresh)
			authz.With(s.rate(s.limiter.Login)).Post("/desktop/revoke", s.handleDesktopRevoke)

			// Everything below needs a session. Group keeps the middleware scoped to these
			// routes, since chi forbids Use() after routes have been registered on a mux.
			authz.Group(func(privileged chi.Router) {
				privileged.Use(s.RequireAuth)
				privileged.With(s.rate(s.limiter.Login)).Get("/me", s.handleCurrentUser)
				privileged.With(s.rate(s.limiter.Login)).Post("/desktop/bridge-token", s.handleBridgeToken)
				privileged.With(s.rate(s.limiter.Login)).Post("/password/change", s.handleChangePassword)
				privileged.With(s.rate(s.limiter.Account)).Get("/sessions", s.handleListSessions)
				privileged.With(s.rate(s.limiter.Account)).Delete("/sessions/{id}", s.handleRevokeSession)
			})
		})

		// Playback. The stream route authenticates with either a bearer token or a signed
		// ticket, because a media element cannot set a header.
		api.With(s.rate(s.limiter.StreamTicket), s.RequireAuth).Get("/episodes/{id}/stream-ticket", s.handleStreamTicket)
		api.With(s.rate(s.limiter.MediaStream)).Get("/episodes/{id}/stream", s.handleStream)
		api.With(s.rate(s.limiter.HLSSession), s.RequireAuth).Post("/episodes/{id}/hls-session", s.handleCreateHLSSession)
		api.With(s.rate(s.limiter.PublicRead), s.RequireAuth).Get("/episodes/{id}/subtitles", s.handleEpisodeSubtitles)
		api.With(s.rate(s.limiter.PublicRead), s.RequireAuth).Get("/episodes/{id}/audio-tracks", s.handleEpisodeAudioTracks)

		// HLS playlist and segments are addressed by an opaque session id that only the
		// create call can mint, so they stay unauthenticated but unguessable.
		api.With(s.rate(s.limiter.HLSSegment)).Get("/hls/{sessionId}/playlist.m3u8", s.handleHLSPlaylist)
		api.With(s.rate(s.limiter.HLSSegment)).Get("/hls/{sessionId}/{segment}", s.handleHLSSegment)
		api.With(s.rate(s.limiter.HLSSession), s.RequireAuth).Post("/hls/{sessionId}/seek", s.handleHLSSeek)
		api.With(s.rate(s.limiter.HLSSession), s.RequireAuth).Post("/hls/{sessionId}/heartbeat", s.handleHLSHeartbeat)
		api.With(s.rate(s.limiter.HLSSession), s.RequireAuth).Delete("/hls/{sessionId}", s.handleDestroyHLSSession)

		// Library rescan, kept under /api/rescan for the existing clients.
		api.With(s.rate(s.limiter.Rescan), s.RequireAuth, s.RequireAdmin).Post("/rescan", s.handleRescan)

		// User-owned data. These replace the browser's direct database access.
		api.Route("/me", func(me chi.Router) {
			me.Use(s.RequireAuth, MaxBody(maxJSONBody))
			// "/" is the mount point itself, so these answer on /api/me exactly.
			me.With(s.rate(s.limiter.Account)).Delete("/", s.handleDeleteAccount)
			me.With(s.rate(s.limiter.UserData)).Patch("/", s.handleUpdateProfile)
			me.With(s.rate(s.limiter.UserData)).Get("/watchlist", s.handleListWatchlist)
			me.With(s.rate(s.limiter.UserData)).Put("/watchlist/{animeId}", s.handlePutWatchlist)
			me.With(s.rate(s.limiter.UserData)).Patch("/watchlist/{animeId}", s.handlePatchWatchlist)
			me.With(s.rate(s.limiter.UserData)).Delete("/watchlist/{animeId}", s.handleDeleteWatchlist)
			me.With(s.rate(s.limiter.UserData)).Get("/progress", s.handleGetProgress)
			me.With(s.rate(s.limiter.UserData)).Put("/progress", s.handlePutProgress)
			me.With(s.rate(s.limiter.UserData)).Get("/progress/recent", s.handleRecentProgress)
			me.With(s.rate(s.limiter.UserData)).Get("/history", s.handleListHistory)
			me.With(s.rate(s.limiter.UserData)).Put("/history", s.handlePutHistory)
			me.With(s.rate(s.limiter.UserData)).Get("/preferences", s.handleGetPreferences)
			me.With(s.rate(s.limiter.UserData)).Put("/preferences", s.handlePutPreferences)
		})

		// Watch party lookup over HTTP, so a client can validate a code before asking the
		// user to open the matching episode.
		api.With(s.rate(s.limiter.PublicRead), s.RequireAuth).Get("/syncplay/rooms/{code}", s.handleRoomLookup)

		// Torrent search and downloads. Both are local-storage features: in s3 mode
		// the handlers answer 501 downloads_unavailable. "events" must be registered
		// before "{id}" or chi captures it as a job id.
		api.With(s.rate(s.limiter.TorrentSearch), s.RequireAuth).Get("/torrents/search", s.handleTorrentSearch)
		api.With(s.rate(s.limiter.DownloadCreate), s.RequireAuth, MaxBody(maxJSONBody)).Post("/downloads", s.handleDownloadCreate)
		api.With(s.rate(s.limiter.DownloadRead), s.RequireAuth).Get("/downloads", s.handleDownloadList)
		api.With(s.rate(s.limiter.DownloadEvents), s.RequireAuth).Get("/downloads/events", s.handleDownloadEvents)
		api.With(s.rate(s.limiter.DownloadRead), s.RequireAuth).Get("/downloads/{id}", s.handleDownloadGet)
		api.With(s.rate(s.limiter.DownloadWrite), s.RequireAuth, MaxBody(maxJSONBody)).Patch("/downloads/{id}", s.handleDownloadPatch)
		api.With(s.rate(s.limiter.DownloadWrite), s.RequireAuth).Delete("/downloads/{id}", s.handleDownloadDelete)
		api.With(s.rate(s.limiter.Trending), s.RequireAuth).Get("/search/trending", s.handleSearchTrending)

		// Administration.
		api.Route("/admin", func(admin chi.Router) {
			admin.Use(s.RequireAuth, s.RequireAdmin, MaxBody(maxJSONBody))
			admin.With(s.rate(s.limiter.Admin)).Get("/users", s.handleAdminUsers)
			admin.With(s.rate(s.limiter.Admin)).Patch("/users/{id}", s.handleAdminSetUserAdmin)
			admin.With(s.rate(s.limiter.Admin)).Delete("/shows/{id}", s.handleAdminDeleteShow)
			admin.With(s.rate(s.limiter.Admin)).Post("/scan", s.handleAdminScan)
			admin.With(s.rate(s.limiter.Admin)).Get("/metrics", s.handleAdminMetrics)
			admin.With(s.rate(s.limiter.Admin)).Post("/library/cleanup", s.handleAdminCleanupCaches)
		})
	})

	// Watch party sockets live under the same /api prefix as the REST routes.
	if s.hub != nil {
		s.hub.Register(r)
	}

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, http.StatusNotFound, "Not found.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed.")
	})
	return r
}

// ListenAndServe starts the HTTP server. It returns once the listener is closed, so the
// caller blocks here until Shutdown or a fatal error.
func (s *Server) ListenAndServe() error {
	s.httpServer = &http.Server{
		Addr:         s.cfg.Addr(),
		Handler:      s.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // Media responses stream for as long as the client watches.
		IdleTimeout:  120 * time.Second,
		// 1 MiB header budget keeps a request line attack bounded.
		MaxHeaderBytes: 1 << 20,
	}
	s.logger.Info("server listening",
		slog.String("addr", s.cfg.Addr()),
		slog.String("env", s.cfg.Env),
		slog.String("storage_mode", string(s.cfg.Storage.Mode)))

	if err := s.startMetricsListener(); err != nil {
		return err
	}

	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// startMetricsListener opens the scrape port. A failure here is fatal: a server that cannot
// be observed is a deployment that will be debugged by guessing.
func (s *Server) startMetricsListener() error {
	if s.metrics == nil {
		return nil
	}
	// Bound synchronously so a port conflict is a startup failure, not a dashboard with no
	// target three minutes later.
	listener, err := net.Listen("tcp", s.cfg.Metrics.Addr)
	if err != nil {
		return fmt.Errorf("httpx: metrics listener on %s: %w", s.cfg.Metrics.Addr, err)
	}
	s.metricsServer = &http.Server{
		Handler:      s.metricsHandler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	s.logger.Info("metrics listening", slog.String("addr", s.cfg.Metrics.Addr),
		slog.String("path", s.cfg.Metrics.Path))

	go func() {
		if err := s.metricsServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("metrics listener stopped", slog.String("error", err.Error()))
		}
	}()
	return nil
}

// Shutdown stops accepting connections and lets in-flight media streams finish.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.metricsServer != nil {
		_ = s.metricsServer.Shutdown(ctx)
	}
	if s.httpServer == nil {
		return nil
	}
	err := s.httpServer.Shutdown(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// ── Shared handler helpers ───────────────────────────────────────────────────

// requirePrincipal resolves the authenticated caller or writes the failure response.
func (s *Server) requirePrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	principal := PrincipalFromContext(r.Context())
	if principal == nil {
		WriteError(w, http.StatusUnauthorized, "Missing or invalid authorization token.")
		return nil, false
	}
	return principal, true
}

// setSessionCookie installs the browser session cookie.
//
// HttpOnly keeps the token out of page scripts, so an XSS in the site cannot read it; the
// SPA that prefers a bearer header receives it in the login response instead.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time, remember bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
	_ = remember
}

// clearSessionCookie expires the cookie immediately.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.Auth.SecureCookies, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(0, 0), MaxAge: -1,
	})
}

// parsePositiveInt reads a query parameter as a positive integer.
func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// serveLocalFile writes a local media file honouring a single HTTP range request.
//
// Range support is what makes seeking work in both the browser player and mpv, and chunk
// capping keeps one connection from holding a worker for the whole file. A client that asks
// past the end gets 416 with the total size so it can restart its request.
func serveLocalFile(w http.ResponseWriter, r *http.Request, file io.ReadSeeker, size int64, mimeType string, chunkSize int64, cacheable bool) {
	header := w.Header()
	header.Set("Content-Type", mimeType)
	header.Set("Accept-Ranges", "bytes")
	if cacheable {
		header.Set("Cache-Control", "private, max-age=120")
	} else {
		header.Set("Cache-Control", "no-store")
	}
	header.Set("X-Content-Type-Options", "nosniff")

	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		header.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, file)
		return
	}

	start, end, err := parseRange(rangeHeader, size, chunkSize)
	if err != nil {
		header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		WriteError(w, http.StatusRequestedRangeNotSatisfiable, "Range is not satisfiable.")
		return
	}

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not seek in the media file.")
		return
	}
	length := end - start + 1
	header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	header.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	_, _ = io.CopyN(w, file, length)
}

// parseRange resolves a single-range header to a concrete byte span.
func parseRange(header string, size, chunkSize int64) (start, end int64, err error) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, errors.New("unsupported range unit")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if strings.Contains(spec, ",") {
		// Multi-range requests are not needed by either player; answering with the first
		// span would be wrong, so they are refused.
		return 0, 0, errors.New("only a single range is supported")
	}
	rawStart, rawEnd, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, errors.New("malformed range")
	}

	switch {
	case rawStart == "" && rawEnd == "":
		return 0, 0, errors.New("malformed range")
	case rawStart == "":
		// A suffix range ("bytes=-500") means the last 500 bytes.
		suffix, err := strconv.ParseInt(rawEnd, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, errors.New("malformed suffix range")
		}
		if suffix > size {
			suffix = size
		}
		startOffset := size - suffix
		endOffset := size - 1
		if chunkSize > 0 && endOffset-startOffset+1 > chunkSize {
			endOffset = startOffset + chunkSize - 1
		}
		return startOffset, endOffset, nil
	default:
		startOffset, err := strconv.ParseInt(rawStart, 10, 64)
		if err != nil || startOffset < 0 || startOffset >= size {
			return 0, 0, errors.New("range start is outside the file")
		}
		endOffset := size - 1
		if rawEnd != "" {
			requested, err := strconv.ParseInt(rawEnd, 10, 64)
			if err != nil || requested < startOffset {
				return 0, 0, errors.New("range end is invalid")
			}
			if requested < endOffset {
				endOffset = requested
			}
		}
		if chunkSize > 0 && endOffset-startOffset+1 > chunkSize {
			endOffset = startOffset + chunkSize - 1
		}
		return startOffset, endOffset, nil
	}
}
