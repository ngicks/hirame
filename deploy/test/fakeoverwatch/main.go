// Command overwatch is a stand-in for the go-overwatch daemon, for
// environments where the real one cannot start.
//
// fanotify filesystem marks require CAP_SYS_ADMIN in the *initial* user
// namespace. The end-to-end harness runs inside a nested user namespace where
// no depth can supply that, so overwatch.container's real image refuses to
// start — correctly, and by design (D-009's amendment). This binary answers the
// same OverwatchService on the same unix socket, reading the same
// deploy/config/overwatch.json, and accepting the same two argv shapes
// overwatch.container uses, so the unit under test needs no override but
// Image=.
//
// It is honest about exactly one thing and fakes exactly one thing:
//
//   - Scan really walks the filesystem, so every path, size, mode and mtime the
//     indexer records came from the kernel.
//   - Subscribe emits no file events at all. It reports a GAP_KIND_STREAM_START
//     marker instead, on a timer, which drives the indexer down its Scan
//     reconciliation path — the fallback the D-009 amendment names for
//     environments without fanotify. Latency to notice a change is therefore
//     the gap interval rather than milliseconds.
//
// Nothing downstream is faked: the indexer, River, Tika, Gahaku, VersityGW,
// PostgreSQL and the API are the real deployment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"
)

// version is what Status reports. The "+fake" suffix is deliberate: anything
// reading a status dump has to be able to tell this apart from the daemon.
const version = "0.0.0+fake"

// scanBatchSize bounds one ObservationBatch. The real daemon paces its walk
// against a syscall budget; there is nothing to protect here, so batching is
// only about not sending a message per file.
const scanBatchSize = 128

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fake-overwatch:", err)
		os.Exit(1)
	}
}

// run dispatches the two argv shapes overwatch.container uses: `server serve
// --config <path>` from Exec= and `client --socket <path> status` from
// HealthCmd=. Anything else is a mismatch between this stand-in and the unit,
// which should be loud.
func run(args []string) error {
	if len(args) == 0 {
		return errors.New(
			"usage: overwatch server serve --config <path> | overwatch client --socket <path> status",
		)
	}
	switch args[0] {
	case "server":
		return runServer(args[1:])
	case "client":
		return runClient(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// config is the subset of deploy/config/overwatch.json this stand-in needs.
// The remaining keys (events, limits) describe fanotify behaviour that has no
// meaning without fanotify.
type config struct {
	Instance string `json:"instance"`
	Listen   struct {
		Unix string `json:"unix"`
	} `json:"listen"`
	Watch []struct {
		Root string `json:"root"`
	} `json:"watch"`
}

func runServer(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the overwatch configuration")
	// Every subscription reports a gap, and the indexer answers a gap with a
	// full rescan, so this is the whole system's change-detection latency.
	gapInterval := flags.Duration(
		"gap-interval",
		envDuration("FAKE_OVERWATCH_GAP_INTERVAL", 10*time.Second),
		"how often Subscribe reports a STREAM_START gap",
	)
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("serve: --config is required")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	socket := cfg.Listen.Unix
	if socket == "" {
		return fmt.Errorf("%s: listen.unix is empty", *configPath)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	// A socket left behind by an earlier container would make Listen fail with
	// EADDRINUSE even though nothing holds it.
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socket, err)
	}
	// 0660 with the process's own group, matching what the daemon does and what
	// indexer.container's User=10001:0 is written against.
	if err := os.Chmod(socket, 0o660); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	roots := make([]string, 0, len(cfg.Watch))
	for _, w := range cfg.Watch {
		roots = append(roots, filepath.Clean(w.Root))
	}

	server := grpc.NewServer()
	overwatchv1.RegisterOverwatchServiceServer(server, &service{
		roots:       roots,
		gapInterval: *gapInterval,
		logger:      logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	logger.Info("fake overwatch serving",
		slog.String("instance", cfg.Instance),
		slog.String("socket", socket),
		slog.Any("roots", roots),
		slog.Duration("gap_interval", *gapInterval),
	)
	return server.Serve(listener)
}

// runClient implements the one client call overwatch.container's HealthCmd
// makes. It is a readiness probe, so it reports only whether Status answered.
func runClient(args []string) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	socket := flags.String("socket", "", "path to the daemon's unix socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.Arg(0) != "status" {
		return fmt.Errorf("client: unsupported subcommand %q", flags.Arg(0))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	resp, err := overwatchv1.NewOverwatchServiceClient(conn).
		Status(ctx, &overwatchv1.StatusRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("version=%s roots=%d ring_head_seq=%d\n",
		resp.GetVersion(), len(resp.GetRoots()), resp.GetRingHeadSeq())
	return nil
}

type service struct {
	overwatchv1.UnimplementedOverwatchServiceServer

	roots       []string
	gapInterval time.Duration
	logger      *slog.Logger

	// seq advances once per emitted gap and is what Status reports as the ring
	// head. The indexer stores it as its watermark after each reconcile, so a
	// head that never moved would make every subscription look like a replay
	// from the beginning.
	seq atomic.Uint64
}

func (s *service) Status(
	_ context.Context,
	_ *overwatchv1.StatusRequest,
) (*overwatchv1.StatusResponse, error) {
	roots := make([]*overwatchv1.WatchRootStatus, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, &overwatchv1.WatchRootStatus{
			Root: root,
			// marked=false with a reason, rather than a claim of a mark that
			// does not exist: an operator reading this must not believe events
			// are coming.
			Marked: false,
			Error:  "fake daemon: no fanotify mark; Subscribe reports gaps and Scan walks the tree",
		})
	}
	return &overwatchv1.StatusResponse{
		Version:     version,
		Roots:       roots,
		RingHeadSeq: s.seq.Load(),
		RingTailSeq: 0,
	}, nil
}

// Subscribe never yields a FileEvent. It reports a STREAM_START gap now and
// every gapInterval after, which is what makes the indexer rescan.
func (s *service) Subscribe(
	_ *overwatchv1.SubscribeRequest,
	stream grpc.ServerStreamingServer[overwatchv1.SubscribeResponse],
) error {
	ticker := time.NewTicker(s.gapInterval)
	defer ticker.Stop()
	for {
		if err := s.sendGap(stream); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *service) sendGap(
	stream grpc.ServerStreamingServer[overwatchv1.SubscribeResponse],
) error {
	seq := s.seq.Add(1)
	return stream.Send(&overwatchv1.SubscribeResponse{
		Item: &overwatchv1.SubscribeResponse_Gap{
			Gap: &overwatchv1.GapMarker{
				Seq:  seq,
				Time: timestamppb.Now(),
				Kind: overwatchv1.GapKind_GAP_KIND_STREAM_START,
			},
		},
	})
}

// Scan walks root and streams what is actually on disk.
func (s *service) Scan(
	req *overwatchv1.ScanRequest,
	stream grpc.ServerStreamingServer[overwatchv1.ScanResponse],
) error {
	root := filepath.Clean(req.GetRoot())
	if !s.covers(root) {
		return fmt.Errorf("scan: %s is under no configured watch root", root)
	}

	var (
		batch   []*overwatchv1.Observation
		dirs    uint64
		entries uint64
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := stream.Send(&overwatchv1.ScanResponse{
			Item: &overwatchv1.ScanResponse_Batch{
				Batch: &overwatchv1.ObservationBatch{Observations: batch},
			},
		})
		batch = nil
		return err
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is normal; it will be absent from
			// the next scan too, which is how the indexer notices.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			dirs++
		}
		entries++
		batch = append(batch, &overwatchv1.Observation{
			Path: path,
			Stat: statInfo(info),
		})
		if len(batch) >= scanBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	if err := flush(); err != nil {
		return err
	}

	s.logger.Info("scan complete",
		slog.String("root", root),
		slog.Uint64("dirs", dirs),
		slog.Uint64("entries", entries),
	)
	return stream.Send(&overwatchv1.ScanResponse{
		Item: &overwatchv1.ScanResponse_Done{
			Done: &overwatchv1.ScanDone{DirsVisited: dirs, EntriesObserved: entries},
		},
	})
}

func (s *service) covers(path string) bool {
	for _, root := range s.roots {
		if path == root || len(path) > len(root) && path[:len(root)+1] == root+"/" {
			return true
		}
	}
	return false
}

// statInfo mirrors the daemon's own conversion, which casts the fs.FileMode
// straight to uint32 rather than sending the raw st_mode. internal/ingest
// tests the directory bit with fs.ModeDir on the strength of that, so getting
// it wrong here would make every directory look like a file.
func statInfo(info fs.FileInfo) *overwatchv1.StatInfo {
	out := &overwatchv1.StatInfo{
		Size:  uint64(info.Size()),
		Mode:  uint32(info.Mode()),
		Mtime: timestamppb.New(info.ModTime()),
	}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		out.Ino = sys.Ino
		out.Dev = uint64(sys.Dev)
		out.Nlink = uint32(sys.Nlink)
		out.Uid = sys.Uid
		out.Gid = sys.Gid
		out.Ctime = timestamppb.New(time.Unix(sys.Ctim.Sec, sys.Ctim.Nsec))
	}
	return out
}

func loadConfig(path string) (config, error) {
	var cfg config
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Watch) == 0 {
		return cfg, fmt.Errorf("%s: no watch roots", path)
	}
	return cfg, nil
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}
