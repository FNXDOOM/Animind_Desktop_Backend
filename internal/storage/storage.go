// Package storage abstracts where media files live.
//
// Two modes exist because deployments do: an S3-compatible bucket (R2, Spaces, MinIO,
// AWS) where playback is a redirect to a presigned URL, and a directory on the host where
// playback is a byte-range passthrough served by this process. Callers ask for either a
// URL or a local path and never branch on the mode themselves.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"animind-backend-go/internal/config"
)

// Service resolves and reads media files for the configured mode.
type Service struct {
	cfg       *config.Config
	client    *s3.Client
	presigner *s3.PresignClient
	logger    *slog.Logger
}

// ErrFileNotFound is returned when a local path does not exist, which the scanner treats
// as "this file was deleted from the library".
var ErrFileNotFound = errors.New("storage: file not found")

// New builds the service. In S3 mode credentials are required; in local mode the root
// directory must exist, which is validated here so a typo fails at startup rather than on
// the first playback request.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*Service, error) {
	s := &Service{cfg: cfg, logger: logger}
	if cfg.Storage.Mode != config.StorageS3 {
		info, err := os.Stat(cfg.Storage.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("storage: LOCAL_STORAGE_PATH %q is not readable: %w", cfg.Storage.LocalPath, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("storage: LOCAL_STORAGE_PATH %q is not a directory", cfg.Storage.LocalPath)
		}
		return s, nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(orDefault(cfg.Storage.S3.Region, "auto")),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.Storage.S3.AccessKey, cfg.Storage.S3.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Storage.S3.Endpoint != "" {
			// A custom endpoint implies a third-party service that expects path-style
			// addressing; virtual-host style would resolve to a nonexistent DNS name.
			o.BaseEndpoint = aws.String(cfg.Storage.S3.Endpoint)
			o.UsePathStyle = true
		}
	})
	s.client = client
	s.presigner = s3.NewPresignClient(client)
	return s, nil
}

// Mode reports the active storage backend.
func (s *Service) Mode() config.StorageMode { return s.cfg.Storage.Mode }

// IsLocal reports whether media is read from the host filesystem.
func (s *Service) IsLocal() bool { return s.cfg.Storage.Mode == config.StorageLocal }

// Bucket returns the configured bucket name, or the "local" marker the library rows used
// before this rewrite.
func (s *Service) Bucket() string {
	if s.IsLocal() {
		return "local"
	}
	return s.cfg.Storage.S3.Bucket
}

// Presign produces a time-limited download URL for an object.
//
// The URL carries the signature, so the object store authorizes playback without this
// process staying in the data path.
func (s *Service) Presign(ctx context.Context, key, bucket string) (string, time.Time, error) {
	if s.presigner == nil {
		return "", time.Time{}, errors.New("storage: presigning is unavailable in local mode")
	}
	if bucket == "" {
		bucket = s.cfg.Storage.S3.Bucket
	}
	out, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) { o.Expires = s.cfg.Storage.PresignTTL })
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storage: presign %q: %w", key, err)
	}
	// The expiry is reported from the requested TTL rather than the SDK result: the
	// presigner's output carries the URL only, and clients need a matching timestamp.
	return out.URL, time.Now().Add(s.cfg.Storage.PresignTTL), nil
}

// LocalPath resolves a library-relative path inside the root and refuses anything that
// escapes it. Every local-mode read goes through this, so a path stored in the database
// can never reach /etc or a sibling mount.
//
// Two checks run, in this order: the lexical join, then a symlink resolution on the result.
// Cleaning and prefix-comparing alone is not enough, because a symbolic link placed inside
// the library points the kernel somewhere else after the check passes.
func (s *Service) LocalPath(relative string) (string, error) {
	root, err := filepath.Abs(s.cfg.Storage.LocalPath)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(strings.ReplaceAll(relative, "\\", "/"))
	// Strip any leading separator so an absolute-looking key cannot escape by joining
	// onto the root as an absolute path.
	cleaned = strings.TrimPrefix(cleaned, string(filepath.Separator))
	candidate := filepath.Join(root, filepath.FromSlash(cleaned))

	if !withinRoot(root, candidate) {
		return "", fmt.Errorf("storage: path %q escapes library root", relative)
	}

	// When the target exists, resolve every link on the way to it and re-check. A missing
	// file is left alone: the caller's open will report it, and writes must still be able to
	// create into a fresh subdirectory.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		rootReal := root
		if expanded, rootErr := filepath.EvalSymlinks(root); rootErr == nil {
			rootReal = expanded
		}
		if !withinRoot(rootReal, resolved) {
			return "", fmt.Errorf("storage: path %q resolves outside the library root through a link", relative)
		}
	}
	return candidate, nil
}

// withinRoot reports whether target lives inside root after cleaning.
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// Stat returns a local file's size and modification time.
func (s *Service) Stat(path string) (int64, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, time.Time{}, ErrFileNotFound
		}
		return 0, time.Time{}, err
	}
	return info.Size(), info.ModTime(), nil
}

// OpenLocal opens a file for streaming.
func (s *Service) OpenLocal(path string) (*os.File, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrFileNotFound
		}
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

// ReadFile reads a small local file completely, used for subtitle sidecars.
func (s *Service) ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrFileNotFound
		}
		return nil, err
	}
	return data, nil
}

// IsVideoFile reports whether a path has one of the configured video extensions.
func (s *Service) IsVideoFile(path string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	for _, allowed := range s.cfg.Scanner.VideoExtensions {
		if allowed == ext {
			return true
		}
	}
	return false
}

// ListVideoFiles returns every video path in the library, relative to the root in local
// mode and as an object key in S3 mode.
//
// Local mode skips the audio variant cache directory and the .incoming download staging
// directory: those are derived or incomplete artifacts, and indexing them would double the
// library and create a feedback loop where each scan indexes the previous scan's transcodes
// or a torrent's unfinished download.
func (s *Service) ListVideoFiles(ctx context.Context) ([]string, error) {
	if s.IsLocal() {
		return s.listLocalVideoFiles(ctx, s.cfg.Storage.LocalPath, s.cfg.Storage.LocalPath)
	}
	return s.listS3VideoFiles(ctx)
}

func (s *Service) listLocalVideoFiles(ctx context.Context, root, current string) ([]string, error) {
	entries, err := os.ReadDir(current)
	if err != nil {
		return nil, fmt.Errorf("storage: read %q: %w", current, err)
	}

	out := make([]string, 0, 64)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		name := filepath.Join(current, entry.Name())
		if entry.IsDir() {
			if entry.Name() == AudioCacheDirName || entry.Name() == DownloadsDirName {
				continue
			}
			nested, err := s.listLocalVideoFiles(ctx, root, name)
			if err != nil {
				s.logger.Warn("could not read library subdirectory",
					slog.String("path", name), slog.String("error", err.Error()))
				continue
			}
			out = append(out, nested...)
			continue
		}
		if !s.IsVideoFile(entry.Name()) {
			continue
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

func (s *Service) listS3VideoFiles(ctx context.Context) ([]string, error) {
	if s.client == nil {
		return nil, errors.New("storage: S3 client is not initialised")
	}
	out := make([]string, 0, 256)
	var token *string
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Storage.S3.Bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("storage: list bucket objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			if s.IsVideoFile(*object.Key) {
				out = append(out, *object.Key)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return out, nil
		}
		token = page.NextContinuationToken
		if token == nil {
			return out, nil
		}
	}
}

// AudioCacheDirName is the hidden directory holding derived audio variants.
const AudioCacheDirName = ".animind-audio-cache"

// DownloadsDirName is the hidden directory where the download manager stages torrents while
// they are incomplete; the scanner must never index it.
const DownloadsDirName = ".incoming"

// SubtitleDirName is the canonical folder name for extracted subtitles.
const SubtitleDirName = "Subtitles"

// ListKeysUnder returns object keys beneath a prefix in S3 mode, used to find subtitle
// sidecars that sit next to a video file.
func (s *Service) ListKeysUnder(ctx context.Context, prefix string) ([]string, error) {
	if s.IsLocal() {
		dir, err := s.LocalPath(prefix)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				out = append(out, strings.TrimSuffix(prefix, "/")+"/"+e.Name())
			}
		}
		return out, nil
	}

	out := make([]string, 0, 16)
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.cfg.Storage.S3.Bucket), Prefix: aws.String(prefix)}
	for {
		page, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("storage: list under prefix: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				out = append(out, *object.Key)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated || page.NextContinuationToken == nil {
			return out, nil
		}
		input.ContinuationToken = page.NextContinuationToken
	}
}

// GetObject reads a small object completely (subtitle sidecars, not video).
func (s *Service) GetObject(ctx context.Context, key, bucket string) ([]byte, error) {
	if s.IsLocal() {
		path, err := s.LocalPath(key)
		if err != nil {
			return nil, err
		}
		return s.ReadFile(path)
	}
	if bucket == "" {
		bucket = s.cfg.Storage.S3.Bucket
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	defer out.Body.Close()
	// 8 MiB is far above any subtitle file and far below a video, so a mis-keyed request
	// cannot turn this into a memory-exhaustion vector.
	data, err := io.ReadAll(io.LimitReader(out.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("storage: read %q: %w", key, err)
	}
	return data, nil
}

// EnsureParentDir creates the directory for a target path, used before writing extracted
// subtitles or cached audio.
func EnsureParentDir(path string) error { return os.MkdirAll(filepath.Dir(path), 0o755) }

// WriteFileAtomic writes bytes through a temporary file and renames it into place, so a
// reader never observes a half-written subtitle or cached variant.
func WriteFileAtomic(path string, data []byte) error {
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
