// Package config resolves the process configuration of both search-api
// binaries from the environment.
//
// Environment only, and read in exactly one place: the deployment is Quadlet
// units whose configuration surface is an env file, so a config-file layer
// would be a second source of truth nothing writes to.
package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the union of what both entrypoints need. It is loaded whole by
// each of them; a binary simply leaves the sections it does not use alone,
// which keeps one env file able to configure the whole pod.
type Config struct {
	Server      Server
	Database    Database
	Tika        Tika
	Gahaku      Gahaku
	Storage     Storage
	Watch       Watch
	Concurrency Concurrency
}

// Server configures the ConnectRPC HTTP server.
type Server struct {
	// ListenAddr is bound by search-api. The GUI's reverse proxy is the only
	// expected client (D-010), so this stays on the internal network.
	ListenAddr string `env:"LISTEN_ADDR" envDefault:":8080"`
	// ShutdownTimeout bounds the wait for in-flight requests after a signal.
	// It sits above a typical render so an open stream finishes.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"30s"`
	// ReadHeaderTimeout guards against a client that opens a connection and
	// never completes its request headers.
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"10s"`
}

// Database configures the PostgreSQL connection shared by the API, the River
// client, and the workers.
type Database struct {
	// URL is required because neither binary can do anything useful without
	// it: the index, the job queue, and the render-cache accounting all live
	// in PostgreSQL.
	URL string `env:"DATABASE_URL,required,notEmpty"`
	// MaxConns caps the pool. 0 leaves the driver default.
	MaxConns int32 `env:"DATABASE_MAX_CONNS"`
}

// Tika configures the text and metadata extraction service.
type Tika struct {
	BaseURL string `env:"TIKA_URL"`
	// Timeout bounds one extraction request. Large scanned documents are slow,
	// so this is generous compared with an ordinary HTTP client default.
	Timeout time.Duration `env:"TIKA_TIMEOUT" envDefault:"5m"`
	// MaxBytes caps the document size sent for extraction. 0 disables the cap.
	MaxBytes int64 `env:"TIKA_MAX_BYTES"`
}

// Gahaku configures the document renderer.
type Gahaku struct {
	// Target is a gRPC dial target; Gahaku speaks gRPC, not Connect.
	Target string `env:"GAHAKU_URL"`
	// Timeout bounds one render stream end to end.
	Timeout time.Duration `env:"GAHAKU_TIMEOUT" envDefault:"5m"`
}

// Storage configures the S3 gateway holding the thumbnail cache.
type Storage struct {
	Endpoint        string `env:"S3_ENDPOINT"`
	Region          string `env:"S3_REGION"          envDefault:"us-east-1"`
	AccessKeyID     string `env:"S3_ACCESS_KEY_ID"`
	SecretAccessKey string `env:"S3_SECRET_ACCESS_KEY"`
	Bucket          string `env:"S3_BUCKET"          envDefault:"hirame-thumbnails"`
	// UsePathStyle defaults on: VersityGW is addressed by endpoint and bucket
	// path, not by a virtual host per bucket.
	UsePathStyle bool `env:"S3_USE_PATH_STYLE" envDefault:"true"`
	// MaxCacheBytes is the cache-wide quota enforced by the maintenance job
	// (D-008). 0 disables the quota.
	MaxCacheBytes int64 `env:"S3_MAX_CACHE_BYTES"`
	// MaxObjectAge evicts thumbnails older than this regardless of the quota.
	// 0 disables age-based eviction.
	MaxObjectAge time.Duration `env:"S3_MAX_OBJECT_AGE"`
	// EvictionInterval is how often the maintenance job enforces the two
	// limits above. It is far shorter than a typical MaxObjectAge because the
	// quota can be breached at any moment by a burst of renders.
	EvictionInterval time.Duration `env:"S3_EVICTION_INTERVAL" envDefault:"15m"`
}

// Watch configures the filesystem side of the indexer.
type Watch struct {
	// Mountpoints are the roots go-overwatch observes, comma-separated. They
	// are bind-mounted read-only into the indexer container (D-009).
	Mountpoints []string `env:"WATCH_MOUNTPOINTS" envSeparator:","`
	// DebounceInterval coalesces bursts of events for one path before a job is
	// enqueued. Filesystem events arrive duplicated and out of order, so this
	// trades latency for far fewer redundant extractions.
	DebounceInterval time.Duration `env:"WATCH_DEBOUNCE_INTERVAL" envDefault:"2s"`
	// OverwatchTarget is a gRPC dial target for the watcher daemon. It wins
	// over OverwatchSocket when set, which is what a daemon reachable over TCP
	// rather than a shared unix socket needs.
	OverwatchTarget string `env:"OVERWATCH_TARGET"`
	// OverwatchSocket is the daemon's unix socket. The default is the path the
	// daemon derives from its own default instance name.
	OverwatchSocket string `env:"OVERWATCH_SOCKET" envDefault:"/run/overwatch/default/default.sock"`
	// ReconnectBackoff is the delay before re-subscribing after the stream
	// drops. The daemon is a separate container that may restart under the
	// indexer, so this is a normal condition rather than a failure.
	ReconnectBackoff time.Duration `env:"WATCH_RECONNECT_BACKOFF" envDefault:"5s"`
	// IncludeExtensions restricts which files become documents. Empty selects
	// the conservative built-in set of what Tika extracts and Gahaku renders.
	IncludeExtensions []string `env:"WATCH_INCLUDE_EXTENSIONS" envSeparator:","`
}

// Concurrency holds the knobs that bound resource use per process, so the API
// and the indexer can be tuned independently (D-011).
type Concurrency struct {
	// RiverWorkers is the total River job concurrency in the indexer.
	// 0 selects a value derived from the CPU count.
	RiverWorkers int `env:"RIVER_WORKERS"`
	// Extract caps simultaneous Tika requests.
	Extract int `env:"EXTRACT_CONCURRENCY" envDefault:"4"`
	// Render caps simultaneous Gahaku render streams, counted per process:
	// both binaries render, and Gahaku's own queue is the shared bottleneck.
	Render int `env:"RENDER_CONCURRENCY" envDefault:"2"`
}

// Load reads the process environment into a Config.
//
// Only DATABASE_URL is mandatory. The service endpoints stay optional so a
// binary can start against a partially provisioned pod and fail on the call
// that actually needs the missing dependency, rather than refusing to start
// and hiding which dependency is absent.
func Load() (Config, error) {
	return LoadFrom(nil)
}

// LoadFrom resolves against an explicit environment. A nil map selects the
// process environment, so Load is LoadFrom(nil); a non-empty map replaces it
// outright, which is how a deployment's env file can be validated without
// exporting any of it.
func LoadFrom(environ map[string]string) (Config, error) {
	var cfg Config
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: environ}); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}
