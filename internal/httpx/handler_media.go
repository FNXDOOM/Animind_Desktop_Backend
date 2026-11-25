package httpx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/media"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/storage"
)

// Playback endpoints. A request unlocks with a session or desktop bearer token, or a signed
// playback ticket in the query string: neither a <video> element nor mpv can attach an
// Authorization header, and a ticket is scoped to one episode, not to an account credential.

// streamTicketResponse is what both clients read before asking for bytes.
type streamTicketResponse struct {
	// URL is relative to this API, which the desktop player resolves against its configured
	// base URL and the client resolves against the API origin.
	URL         string `json:"url"`
	ExpiresIn   int64  `json:"expiresIn"`
	HLSRequired bool   `json:"hlsRequired"`
	ClientType  string `json:"clientType"`
	Message     string `json:"message"`
}

func (s *Server) handleStreamTicket(w http.ResponseWriter, r *http.Request) {
	episodeID := Param(r, "id")
	episode, err := s.store.EpisodeByID(r.Context(), episodeID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Episode not found.")
			return
		}
		s.serverError(w, "Could not prepare playback.", err)
		return
	}

	clientType := ClientHint(r)
	native := clientType == "native"

	// A browser asked to pick a non-default audio track needs a remuxed file; the native
	// player demuxes the original itself and must not be given a transcoded copy.
	var audioTrack *int
	if raw := r.URL.Query().Get("at"); raw != "" {
		if index, parseErr := strconv.Atoi(raw); parseErr == nil && index >= 0 {
			if !native {
				audioTrack = &index
			}
		}
	}

	clientMode := auth.ClientBrowser
	if native {
		clientMode = auth.ClientNative
	}
	ticket, err := s.signer.IssueStreamTicket(episode.ID, audioTrack, clientMode, time.Now().Add(s.cfg.Storage.TicketTTL))
	if err != nil {
		s.serverError(w, "Could not sign the playback ticket.", err)
		return
	}

	query := "?st=" + ticket
	if audioTrack != nil {
		query += "&at=" + strconv.Itoa(*audioTrack)
	}
	url := "/api/episodes/" + episode.ID + "/stream" + query

	message := "Direct stream available."
	if native {
		message = "Native passthrough: the desktop player demuxes the source file."
	}

	WriteJSON(w, http.StatusOK, streamTicketResponse{
		URL:         url,
		ExpiresIn:   int64(s.cfg.Storage.TicketTTL.Seconds()),
		HLSRequired: s.hlsRequiredFor(episode),
		ClientType:  clientType,
		Message:     message,
	})
}

// hlsRequiredFor reports whether a browser needs the HLS path: the container decision is the
// media package's, the mode decision ours, since S3 mode always uses the presigned URL.
func (s *Server) hlsRequiredFor(episode *repository.Episode) bool {
	return s.cfg.IsLocal() && !media.BrowserCanProgressivePlay(episode.FilePath)
}

// authorizeStream decides whether a byte request may proceed. It returns the client mode
// from a ticket when one was presented.
func (s *Server) authorizeStream(w http.ResponseWriter, r *http.Request, episodeID string) (bool, *auth.StreamTicketPayload) {
	ticketValue := r.URL.Query().Get("st")
	if ticketValue != "" {
		payload, err := s.signer.VerifyStreamTicket(ticketValue, episodeID)
		if err == nil {
			return true, payload
		}
		if s.credential(r) == "" {
			WriteError(w, http.StatusUnauthorized, "Playback ticket is invalid or has expired.")
			return false, nil
		}
	}

	principal, err := s.auth.Authenticate(auth.WithClient(r.Context(), auth.Client{
		UserAgent: r.UserAgent(), IPAddress: ClientIP(r, s.cfg.TrustProxy),
	}), s.credential(r))
	if err != nil || principal == nil {
		WriteError(w, http.StatusUnauthorized, "Missing or invalid authorization token.")
		return false, nil
	}
	return true, nil
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	episodeID := Param(r, "id")
	authorized, ticket := s.authorizeStream(w, r, episodeID)
	if !authorized {
		return
	}

	episode, err := s.store.EpisodeByID(r.Context(), episodeID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Episode not found.")
			return
		}
		s.serverError(w, "Could not load the episode.", err)
		return
	}

	// A ticket tells us who asked; a bearer request only tells us by its client hint.
	native := ticket != nil && ticket.CM == auth.ClientNative
	if ticket == nil {
		native = IsNativeClient(r)
	}

	if !s.cfg.IsLocal() {
		bucket := episode.BucketName
		if bucket == "" || bucket == "local" {
			bucket = s.cfg.Storage.S3.Bucket
		}
		url, expiresAt, err := s.storage.Presign(r.Context(), episode.FilePath, bucket)
		if err != nil {
			s.serverError(w, "Could not sign the storage URL.", err)
			return
		}
		// The web client asks for JSON so it can decide between hls.js and a direct source; the
		// desktop player and any media element follow the redirect instead.
		if acceptsJSON(r) {
			WriteJSON(w, http.StatusOK, map[string]any{
				"type": "redirect", "url": url, "expiresIn": int64(time.Until(expiresAt).Seconds()),
			})
			return
		}
		w.Header().Set("Location", url)
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusFound)
		return
	}

	sourcePath, err := s.storage.LocalPath(episode.FilePath)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Episode file is not available.")
		return
	}

	requestedTrack, trackRequested := ticketAudioTrack(ticket, r)
	deliveryPath := sourcePath
	mimeType := mimeTypeForFile(sourcePath)

	// A browser that selected an audio track its codecs cannot decode needs the cached
	// remux. The native player is always served the original file so it can pick tracks
	// itself and keep subtitles in the container.
	if trackRequested && !native && s.audio != nil && s.audio.Available() {
		tracks, probeErr := s.audio.EmbeddedAudioTracks(r.Context(), sourcePath, episode.ID)
		if probeErr == nil {
			browserSafe := false
			for _, track := range tracks {
				if track.StreamIndex == requestedTrack {
					browserSafe = track.BrowserSupported
					break
				}
			}
			if variant, variantErr := s.audio.GetOrCreateVariant(r.Context(), sourcePath, episode.ID, requestedTrack, browserSafe); variantErr == nil {
				deliveryPath = variant
				mimeType = "video/mp4"
			} else {
				s.logger.Warn("audio variant could not be prepared",
					slog.String("episode", episode.ID), slog.Int("track", requestedTrack),
					slog.String("error", variantErr.Error()))
			}
		}
	}

	file, size, err := s.storage.OpenLocal(deliveryPath)
	if err != nil {
		if errors.Is(err, storage.ErrFileNotFound) {
			WriteError(w, http.StatusNotFound, "Episode file is not available.")
			return
		}
		s.serverError(w, "Could not open the media file.", err)
		return
	}
	defer file.Close()

	chunkSize := s.cfg.Storage.RangeChunkSize
	if native {
		chunkSize = s.cfg.Storage.NativeRangeChunkSize
	}
	serveLocalFile(w, r, file, size, mimeType, chunkSize, native)
}

// ticketAudioTrack prefers the ticket's bound track, then an explicit query parameter.
func ticketAudioTrack(ticket *auth.StreamTicketPayload, r *http.Request) (int, bool) {
	if ticket != nil && ticket.At != nil {
		return *ticket.At, true
	}
	raw := r.URL.Query().Get("at")
	if raw == "" {
		return 0, false
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func acceptsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "video/")
}

// mimeTypeForFile maps a container extension to a Content-Type both players accept.
func mimeTypeForFile(path string) string {
	switch media.ExtensionOf(path) {
	case "mp4", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mkv":
		return "video/x-matroska"
	case "avi":
		return "video/x-msvideo"
	case "ts":
		return "video/mp2t"
	case "m3u8":
		return "application/vnd.apple.mpegurl"
	default:
		return "video/mp4"
	}
}

// ── Subtitles and audio tracks ───────────────────────────────────────────────

func (s *Server) handleEpisodeSubtitles(w http.ResponseWriter, r *http.Request) {
	episode, ok := s.loadEpisodeForRead(w, r)
	if !ok {
		return
	}
	tracks, err := s.media.SubtitleTracks(r.Context(), s.storage, episode.FilePath, episode.SeasonNumber, episodeNumberValue(episode))
	if err != nil {
		s.logger.Warn("subtitle lookup failed",
			slog.String("episode", episode.ID), slog.String("error", err.Error()))
		WriteJSON(w, http.StatusOK, map[string]any{"tracks": []media.SubtitleTrack{}})
		return
	}
	if tracks == nil {
		tracks = []media.SubtitleTrack{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"tracks": tracks, "episodeId": episode.ID})
}

func (s *Server) handleEpisodeAudioTracks(w http.ResponseWriter, r *http.Request) {
	episode, ok := s.loadEpisodeForRead(w, r)
	if !ok {
		return
	}
	if !s.cfg.IsLocal() {
		// S3 playback is a redirect to a presigned URL: the browser demuxes the original
		// file, so track selection belongs to the media element, not this API.
		WriteJSON(w, http.StatusOK, map[string]any{"tracks": []media.AudioTrack{}, "mode": "presigned"})
		return
	}
	sourcePath, err := s.storage.LocalPath(episode.FilePath)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Episode file is not available.")
		return
	}
	tracks, err := s.audio.EmbeddedAudioTracks(r.Context(), sourcePath, episode.ID)
	if err != nil {
		s.logger.Warn("audio track probe failed", slog.String("episode", episode.ID), slog.String("error", err.Error()))
		WriteJSON(w, http.StatusOK, map[string]any{"tracks": []media.AudioTrack{}})
		return
	}
	if tracks == nil {
		tracks = []media.AudioTrack{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"tracks": tracks, "episodeId": episode.ID})
}

func (s *Server) loadEpisodeForRead(w http.ResponseWriter, r *http.Request) (*repository.Episode, bool) {
	episode, err := s.store.EpisodeByID(r.Context(), Param(r, "id"))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Episode not found.")
			return nil, false
		}
		s.serverError(w, "Could not load the episode.", err)
		return nil, false
	}
	return episode, true
}

func episodeNumberValue(episode *repository.Episode) float64 {
	if episode.EpisodeNumber == nil {
		return 0
	}
	return *episode.EpisodeNumber
}

// ── HLS sessions ─────────────────────────────────────────────────────────────

type createHLSSessionRequest struct {
	AudioTrackIndex *int    `json:"audioTrackIndex"`
	StartTime       float64 `json:"startTime"`
}

type hlsSessionResponse struct {
	SessionID   string `json:"sessionId"`
	PlaylistURL string `json:"playlistUrl"`
}

// handleCreateHLSSession starts an on-demand remux for a local file. audioTrackIndex is
// optional: clients send no key at all on first play, which means the first audio stream.
func (s *Server) handleCreateHLSSession(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.IsLocal() {
		WriteError(w, http.StatusBadRequest, "HLS sessions are only available in local storage mode.")
		return
	}
	episode, ok := s.loadEpisodeForRead(w, r)
	if !ok {
		return
	}

	var req createHLSSessionRequest
	if r.ContentLength > 0 {
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "audioTrackIndex and startTime must be numbers.")
			return
		}
	}
	trackIndex := 0
	if req.AudioTrackIndex != nil && *req.AudioTrackIndex >= 0 {
		trackIndex = *req.AudioTrackIndex
	}
	startTime := req.StartTime
	if startTime < 0 {
		startTime = 0
	}

	sourcePath, err := s.storage.LocalPath(episode.FilePath)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Episode file is not available.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.HLS.ReadyTimeout+30*time.Second)
	defer cancel()

	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessionID, playlistURL, err := s.hls.Create(ctx, sourcePath, episode.ID, trackIndex, startTime, principal.UserID)
	if err != nil {
		switch {
		case errors.Is(err, media.ErrTooManySessions):
			WriteErrorCode(w, http.StatusTooManyRequests, "hls_capacity",
				"All HLS sessions are busy. Try again in a moment.")
		case errors.Is(err, media.ErrSessionNotReady):
			WriteErrorCode(w, http.StatusGatewayTimeout, "hls_timeout",
				"The player could not start; the server is busy converting this file.")
		default:
			s.logger.Error("HLS session could not start",
				slog.String("episode", episode.ID), slog.String("error", err.Error()))
			WriteErrorDetails(w, http.StatusInternalServerError, "Could not start HLS playback.",
				safeDetail(err, s.cfg.DebugErrors))
		}
		return
	}
	WriteJSON(w, http.StatusOK, hlsSessionResponse{SessionID: sessionID, PlaylistURL: playlistURL})
}

// requireHLS answers the request when HLS sessions are unavailable, which is the case in S3
// mode where the manager is never constructed.
func (s *Server) requireHLS(w http.ResponseWriter) bool {
	if s.hls == nil {
		WriteError(w, http.StatusBadRequest, "HLS sessions are only available in local storage mode.")
		return false
	}
	return true
}

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.requireHLS(w) {
		return
	}
	sessionID := Param(r, "sessionId")
	if !s.hls.Has(sessionID) {
		WriteError(w, http.StatusNotFound, "HLS session not found.")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")

	playlist, err := s.hls.Playlist(r.Context(), sessionID)
	if err != nil {
		// Segments do not exist yet. An empty event playlist is a valid answer that makes a
		// player poll again rather than treating the session as dead.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:"+
			strconv.Itoa(int(s.cfg.HLS.SegmentDuration.Seconds()))+"\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, playlist)
}

func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	if !s.requireHLS(w) {
		return
	}
	sessionID := Param(r, "sessionId")
	if !s.hls.Has(sessionID) {
		WriteError(w, http.StatusNotFound, "HLS session not found.")
		return
	}
	path, err := s.hls.SegmentPath(sessionID, Param(r, "segment"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "Segment is not available yet.")
		return
	}
	file, size, err := s.storage.OpenLocal(path)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Segment is not available yet.")
		return
	}
	defer file.Close()

	// A segment is immutable once written, so it may be cached briefly by the player.
	securityHeadersForMedia(w, true)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

type seekRequest struct {
	Time float64 `json:"time"`
}

func (s *Server) handleHLSSeek(w http.ResponseWriter, r *http.Request) {
	if !s.requireHLS(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req seekRequest
	if err := DecodeJSON(r, &req); err != nil || req.Time < 0 {
		WriteError(w, http.StatusBadRequest, "time must be a non-negative number.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.HLS.ReadyTimeout+30*time.Second)
	defer cancel()
	if err := s.hls.Seek(ctx, Param(r, "sessionId"), req.Time, principal.UserID); err != nil {
		if errors.Is(err, media.ErrUnknownSession) {
			WriteError(w, http.StatusNotFound, "HLS session not found.")
			return
		}
		s.logger.Warn("HLS seek failed", slog.String("session", Param(r, "sessionId")), slog.String("error", err.Error()))
		WriteErrorDetails(w, http.StatusInternalServerError, "Could not seek this stream.",
			safeDetail(err, s.cfg.DebugErrors))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true, "time": req.Time})
}

// handleHLSHeartbeat keeps a session alive across a long buffer gap or a reconnect, which
// the original backend had no way to do.
func (s *Server) handleHLSHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireHLS(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessionID := Param(r, "sessionId")
	if owner, found := s.hls.Owner(sessionID); !found || owner != principal.UserID {
		WriteError(w, http.StatusNotFound, "HLS session not found.")
		return
	}
	if _, err := s.hls.Playlist(r.Context(), sessionID); err != nil {
		// Touching the playlist is what refreshes the idle clock; a session with no playlist
		// yet is still alive.
		s.logger.Debug("HLS heartbeat on a session without a playlist", slog.String("session", sessionID))
	}
	stats, _ := s.hls.SessionStats(sessionID)
	WriteJSON(w, http.StatusOK, map[string]any{"alive": true, "stats": stats})
}

func (s *Server) handleDestroyHLSSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireHLS(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessionID := Param(r, "sessionId")
	if !s.hls.DestroyForUser(r.Context(), sessionID, principal.UserID) {
		WriteError(w, http.StatusNotFound, "HLS session not found.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}
