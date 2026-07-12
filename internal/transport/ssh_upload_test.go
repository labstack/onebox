package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var errTestSessionClosed = errors.New("test session closed")

type blockingWriteCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return 0, errTestSessionClosed
}

func (w *blockingWriteCloser) Close() error {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
	return nil
}

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

type testUploadSession struct {
	stdin       io.WriteCloser
	waitEntered chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
	waitOnce    sync.Once
}

func (s *testUploadSession) StdinPipe() (io.WriteCloser, error) { return s.stdin, nil }
func (s *testUploadSession) Start(string) error                 { return nil }
func (s *testUploadSession) setStderr(io.Writer)                {}

func (s *testUploadSession) Wait() error {
	if s.waitEntered != nil {
		s.waitOnce.Do(func() { close(s.waitEntered) })
	}
	<-s.closed
	return errTestSessionClosed
}

// Close models ssh.Session.Close: it sends a channel-close message but does
// not guarantee local I/O unblocks when the peer is wedged.
func (s *testUploadSession) Close() error { return nil }

func (s *testUploadSession) forceClose() {
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		close(s.closed)
	})
}

type testUploadClient struct {
	session           *testUploadSession
	newSessionEntered chan struct{}
	closed            chan struct{}
	closeOnce         sync.Once
}

func newTestUploadClient(session *testUploadSession) *testUploadClient {
	return &testUploadClient{session: session, closed: make(chan struct{})}
}

func (c *testUploadClient) NewSession() (uploadSession, error) {
	if c.newSessionEntered != nil {
		close(c.newSessionEntered)
		<-c.closed
		return nil, errTestSessionClosed
	}
	return c.session, nil
}

func (c *testUploadClient) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.session != nil {
			c.session.forceClose()
		}
	})
	return nil
}

func uploadFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func awaitUploadResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("upload did not stop after context cancellation")
		return nil
	}
}

func TestUploadCancellationInterruptsArchiveWrite(t *testing.T) {
	stdin := newBlockingWriteCloser()
	sess := &testUploadSession{stdin: stdin, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	localDir := uploadFixture(t)
	done := make(chan error, 1)
	client := newTestUploadClient(sess)
	go func() { done <- uploadWithClient(ctx, client, localDir, "/remote") }()

	select {
	case <-stdin.entered:
	case <-time.After(time.Second):
		t.Fatal("upload never began writing the archive")
	}
	cancel()
	if err := awaitUploadResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload error = %v, want context cancellation", err)
	}
}

func TestUploadCancellationInterruptsRemoteWait(t *testing.T) {
	sess := &testUploadSession{
		stdin:       discardWriteCloser{Writer: io.Discard},
		waitEntered: make(chan struct{}),
		closed:      make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	localDir := uploadFixture(t)
	done := make(chan error, 1)
	client := newTestUploadClient(sess)
	go func() { done <- uploadWithClient(ctx, client, localDir, "/remote") }()

	select {
	case <-sess.waitEntered:
	case <-time.After(time.Second):
		t.Fatal("upload never reached the remote wait")
	}
	cancel()
	if err := awaitUploadResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload error = %v, want context cancellation", err)
	}
}

func TestUploadCancellationInterruptsSessionCreation(t *testing.T) {
	client := newTestUploadClient(nil)
	client.newSessionEntered = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	localDir := uploadFixture(t)
	go func() { done <- uploadWithClient(ctx, client, localDir, "/remote") }()

	select {
	case <-client.newSessionEntered:
	case <-time.After(time.Second):
		t.Fatal("upload never began creating the SSH session")
	}
	cancel()
	if err := awaitUploadResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload error = %v, want context cancellation", err)
	}
}
