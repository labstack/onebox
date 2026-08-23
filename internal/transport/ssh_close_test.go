package transport

import (
	"errors"
	"io"
	"testing"
	"time"
)

type blockingCloser struct {
	release chan struct{}
	closed  chan struct{}
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{release: make(chan struct{}), closed: make(chan struct{}, 1)}
}

func (c *blockingCloser) Close() error {
	c.closed <- struct{}{}
	<-c.release
	return nil
}

type recordingCloser struct {
	closed chan struct{}
	err    error
}

func newRecordingCloser(err error) *recordingCloser {
	return &recordingCloser{closed: make(chan struct{}, 1), err: err}
}

func (c *recordingCloser) Close() error {
	c.closed <- struct{}{}
	return c.err
}

// Closing an SSH client behind a jump host writes through the jump's own
// connection. When that connection has stopped moving bytes the write blocks,
// so the graceful close must not be the only close: dropping the jump's TCP
// connection is what actually releases everything.
func TestCloseStackDropsTheJumpConnectionWhenGracefulCloseBlocks(t *testing.T) {
	graceful := newBlockingCloser()
	raw := newRecordingCloser(nil)
	defer close(graceful.release)

	done := make(chan error, 1)
	go func() { done <- closeStack(raw, 100*time.Millisecond, graceful) }()

	select {
	case <-raw.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the jump connection was never dropped")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close never returned while the graceful close was blocked")
	}
}

func TestCloseStackReportsGracefulErrorsWhenNothingBlocks(t *testing.T) {
	first := newRecordingCloser(errors.New("target went away"))
	second := newRecordingCloser(nil)
	raw := newRecordingCloser(nil)

	err := closeStack(raw, time.Second, first, second)
	if err == nil || !errors.Is(err, first.err) {
		t.Fatalf("close error = %v, want the target's failure", err)
	}
	for _, closer := range []*recordingCloser{first, second, raw} {
		select {
		case <-closer.closed:
		default:
			t.Fatal("a closer was skipped")
		}
	}
}

// An already-closed connection is the expected case: closing the jump client
// closes its connection, so the backstop close finds it gone.
func TestCloseStackIgnoresAnAlreadyClosedJumpConnection(t *testing.T) {
	raw := newRecordingCloser(io.ErrClosedPipe)
	if err := closeStack(raw, time.Second, newRecordingCloser(nil)); err == nil {
		t.Fatal("an unexpected close error was swallowed")
	}
}
