package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/hirame/apps/search-api/internal/config"
)

const testDatabaseURL = "postgres://hirame@localhost:5432/hirame"

func TestLoadFromAppliesDefaults(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{"DATABASE_URL": testDatabaseURL})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"listen addr", cfg.Server.ListenAddr, ":8080"},
		{"shutdown timeout", cfg.Server.ShutdownTimeout, 30 * time.Second},
		{"read header timeout", cfg.Server.ReadHeaderTimeout, 10 * time.Second},
		{"read timeout", cfg.Server.ReadTimeout, 30 * time.Second},
		{"idle timeout", cfg.Server.IdleTimeout, 120 * time.Second},
		{"database url", cfg.Database.URL, testDatabaseURL},
		{"tika timeout", cfg.Tika.Timeout, 5 * time.Minute},
		// Capped by default, unlike TIKA_MAX_BYTES: an uncapped read of the
		// response is the one an operator cannot see going wrong.
		{"tika max response bytes", cfg.Tika.MaxResponseBytes, int64(64 << 20)},
		{"gahaku timeout", cfg.Gahaku.Timeout, 5 * time.Minute},
		{"s3 region", cfg.Storage.Region, "us-east-1"},
		{"s3 bucket", cfg.Storage.Bucket, "hirame-thumbnails"},
		{"s3 path style", cfg.Storage.UsePathStyle, true},
		{"s3 eviction interval", cfg.Storage.EvictionInterval, 15 * time.Minute},
		{"s3 sweep interval", cfg.Storage.SweepInterval, 6 * time.Hour},
		{"s3 timeout", cfg.Storage.Timeout, 30 * time.Second},
		{"watch debounce", cfg.Watch.DebounceInterval, 2 * time.Second},
		{"extract concurrency", cfg.Concurrency.Extract, 4},
		{"render concurrency", cfg.Concurrency.Render, 2},
		{"thumbnail concurrency", cfg.Concurrency.Thumbnail, 4},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadFromReadsEverySection(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{
		"DATABASE_URL":            testDatabaseURL,
		"DATABASE_MAX_CONNS":      "12",
		"LISTEN_ADDR":             "127.0.0.1:9999",
		"SHUTDOWN_TIMEOUT":        "45s",
		"TIKA_URL":                "http://tika:9998",
		"TIKA_MAX_BYTES":          "1048576",
		"TIKA_MAX_RESPONSE_BYTES": "2097152",
		"GAHAKU_URL":              "gahaku:9000",
		"S3_ENDPOINT":             "http://versitygw:7070",
		"S3_ACCESS_KEY_ID":        "key",
		"S3_SECRET_ACCESS_KEY":    "secret",
		"S3_USE_PATH_STYLE":       "false",
		"S3_MAX_CACHE_BYTES":      "2147483648",
		"S3_MAX_OBJECT_AGE":       "720h",
		"S3_EVICTION_INTERVAL":    "5m",
		"S3_SWEEP_INTERVAL":       "12h",
		"S3_TIMEOUT":              "15s",
		"WATCH_MOUNTPOINTS":       "/srv/docs,/srv/scans",
		"RIVER_WORKERS":           "8",
		"EXTRACT_CONCURRENCY":     "3",
		"RENDER_CONCURRENCY":      "1",
		"THUMBNAIL_CONCURRENCY":   "6",
		"READ_HEADER_TIMEOUT":     "5s",
		"READ_TIMEOUT":            "20s",
		"IDLE_TIMEOUT":            "90s",
		"GAHAKU_TIMEOUT":          "2m",
		"TIKA_TIMEOUT":            "90s",
		"WATCH_DEBOUNCE_INTERVAL": "500ms",
		"S3_REGION":               "jp-north-1",
		"S3_BUCKET":               "thumbs",
	})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if want := []string{"/srv/docs", "/srv/scans"}; !slices.Equal(cfg.Watch.Mountpoints, want) {
		t.Errorf("mountpoints = %v, want %v", cfg.Watch.Mountpoints, want)
	}
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"listen addr", cfg.Server.ListenAddr, "127.0.0.1:9999"},
		{"shutdown timeout", cfg.Server.ShutdownTimeout, 45 * time.Second},
		{"read header timeout", cfg.Server.ReadHeaderTimeout, 5 * time.Second},
		{"read timeout", cfg.Server.ReadTimeout, 20 * time.Second},
		{"idle timeout", cfg.Server.IdleTimeout, 90 * time.Second},
		{"database max conns", cfg.Database.MaxConns, int32(12)},
		{"tika url", cfg.Tika.BaseURL, "http://tika:9998"},
		{"tika timeout", cfg.Tika.Timeout, 90 * time.Second},
		{"tika max bytes", cfg.Tika.MaxBytes, int64(1 << 20)},
		{"tika max response bytes", cfg.Tika.MaxResponseBytes, int64(2 << 20)},
		{"gahaku target", cfg.Gahaku.Target, "gahaku:9000"},
		{"gahaku timeout", cfg.Gahaku.Timeout, 2 * time.Minute},
		{"s3 endpoint", cfg.Storage.Endpoint, "http://versitygw:7070"},
		{"s3 region", cfg.Storage.Region, "jp-north-1"},
		{"s3 bucket", cfg.Storage.Bucket, "thumbs"},
		{"s3 access key", cfg.Storage.AccessKeyID, "key"},
		{"s3 secret key", cfg.Storage.SecretAccessKey, "secret"},
		{"s3 path style", cfg.Storage.UsePathStyle, false},
		{"s3 max cache bytes", cfg.Storage.MaxCacheBytes, int64(2 << 30)},
		{"s3 max object age", cfg.Storage.MaxObjectAge, 720 * time.Hour},
		{"s3 eviction interval", cfg.Storage.EvictionInterval, 5 * time.Minute},
		{"s3 sweep interval", cfg.Storage.SweepInterval, 12 * time.Hour},
		{"s3 timeout", cfg.Storage.Timeout, 15 * time.Second},
		{"watch debounce", cfg.Watch.DebounceInterval, 500 * time.Millisecond},
		{"river workers", cfg.Concurrency.RiverWorkers, 8},
		{"extract concurrency", cfg.Concurrency.Extract, 3},
		{"render concurrency", cfg.Concurrency.Render, 1},
		{"thumbnail concurrency", cfg.Concurrency.Thumbnail, 6},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadFromRequiresDatabaseURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ map[string]string
	}{
		{"absent", map[string]string{"LISTEN_ADDR": ":8080"}},
		{"empty", map[string]string{"DATABASE_URL": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadFrom(tc.environ)
			if err == nil {
				t.Fatal("LoadFrom = nil error, want a failure")
			}
			if !strings.Contains(err.Error(), "DATABASE_URL") {
				t.Errorf("error %q does not name DATABASE_URL", err)
			}
		})
	}
}
