package ingest

import (
	"context"
	"fmt"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"
	"github.com/ngicks/go-overwatch/overwatch/pkg/client"
)

// DaemonWatcher adapts the go-overwatch client to [Watcher].
//
// The adapter exists only to narrow the generated streaming types to the Recv
// method the loop uses, which is what lets the loop be tested without a daemon.
type DaemonWatcher struct {
	client *client.Client
}

// DialDaemon connects to the overwatch daemon.
//
// target is a gRPC target string when non-empty and a unix socket path
// otherwise. The dial is lazy, so a daemon that has not started yet surfaces on
// the first RPC rather than here — which is what lets the indexer come up
// alongside it instead of after it.
func DialDaemon(target, socketPath string) (*DaemonWatcher, error) {
	var (
		c   *client.Client
		err error
	)
	if target != "" {
		c, err = client.Dial(target)
	} else {
		c, err = client.DialSocket(socketPath)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to overwatch: %w", err)
	}
	return &DaemonWatcher{client: c}, nil
}

// Close releases the connection.
func (w *DaemonWatcher) Close() error { return w.client.Close() }

// Subscribe implements Watcher.
func (w *DaemonWatcher) Subscribe(
	ctx context.Context,
	req *overwatchv1.SubscribeRequest,
) (EventStream, error) {
	return w.client.Subscribe(ctx, req)
}

// Scan implements Watcher.
func (w *DaemonWatcher) Scan(
	ctx context.Context,
	req *overwatchv1.ScanRequest,
) (ScanStream, error) {
	return w.client.Scan(ctx, req)
}

// Status implements Watcher.
func (w *DaemonWatcher) Status(
	ctx context.Context,
	req *overwatchv1.StatusRequest,
) (*overwatchv1.StatusResponse, error) {
	return w.client.Status(ctx, req)
}

var _ Watcher = (*DaemonWatcher)(nil)
