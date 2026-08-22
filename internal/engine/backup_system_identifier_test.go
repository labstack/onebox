package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestPostgresSystemIdentifierReadsTheLiveClusterIdentity(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "pg_control_system()") {
			return transport.Result{Stdout: "7513211627332151223\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{})

	got, err := engine.PostgresSystemIdentifier(context.Background(), "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if got != "7513211627332151223" {
		t.Fatalf("system identifier = %q", got)
	}
	if command := strings.Join(fake.Commands, "\n"); !strings.Contains(command, "docker exec -u postgres") || !strings.Contains(command, "pg_control_system()") {
		t.Fatalf("identity was not read from the live PostgreSQL cluster:\n%s", command)
	}
}

func TestPostgresSystemIdentifierRejectsUnreadableOutput(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "pg_control_system()") {
			return transport.Result{Stdout: "not-an-identifier\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{})

	if _, err := engine.PostgresSystemIdentifier(context.Background(), "postgres"); err == nil {
		t.Fatal("invalid PostgreSQL system identifier was accepted")
	}
}
