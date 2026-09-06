package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the public CLI through SSH, generated systemd units, real Docker,
// and persistent host files. The fixture owns only its unique app/base path.
func TestServerDurableExecutions(t *testing.T) {
	s := requireServer(t)
	s.requireDocker(t)
	name := fmt.Sprintf("durable%d", time.Now().UnixNano())
	base := "/tmp/onebox-" + name
	root := base + "/" + name
	unit := "ob-" + name + "-refresh"
	dir := t.TempDir()
	s.run(t, "mkdir -p "+base+"/data")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.output(ctx, "systemctl disable --now "+unit+".timer >/dev/null 2>&1; systemctl stop "+unit+".service >/dev/null 2>&1; docker rm -f "+name+"-refresh-1 >/dev/null 2>&1; rm -f /etc/systemd/system/"+unit+".*; systemctl daemon-reload; rm -rf "+base)
	})
	project := fmt.Sprintf(`api_version: onebox.run/v1
app: %s
base_path: %s
environments: {production: {server: %s}}
proxy: {managed: false}
workloads:
  refresh:
    role: job
    image: public.ecr.aws/docker/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
    command: ["true"]
    data_effect: none
    inputs:
      SOURCE: {enum: [catalog, custom], default: catalog}
    volumes:
      - {source: %s/data, path: /data}
    schedule: {cron: "0 0 1 1 *", timeout: 30s, catch_up: false}
    execution:
      retention: 168h
      steps:
        - id: sync
          command:
            - sh
            - -c
            - |
              echo "$SOURCE" >> /data/sync.log
              printf '{"RELEASE":"release-123"}' > "$ONEBOX_OUTPUT_FILE"
          outputs: [RELEASE]
        - id: index
          inputs: {RELEASE_ID: sync.RELEASE}
          command:
            - sh
            - -c
            - |
              echo "$ONEBOX_STEP_ID $ONEBOX_ATTEMPT_ID $RELEASE_ID" >> /data/index.log
              while test -f /data/hold; do touch /data/started; sleep 1; done
              test -f /data/allow
`, name, base, s.target, base)
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	s.deploy(t, dir)
	if out, err := s.ob(t, dir, "schedule", "run", "refresh", "--input", "SOURCE=custom", "--wait"); err == nil {
		t.Fatalf("index should fail before allow marker: %s", out)
	}
	list := s.mustOb(t, dir, "execution", "list", "--output", "json")
	var envelope struct {
		Data struct {
			Executions []map[string]any `json:"executions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(list), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Executions) != 1 {
		t.Fatalf("expected one execution: %s", list)
	}
	record := envelope.Data.Executions[0]
	id, _ := record["id"].(string)
	if id == "" || record["state"] != "failed" {
		t.Fatalf("wrong execution evidence: %s", list)
	}
	s.run(t, "touch "+base+"/data/allow")
	s.mustOb(t, dir, "execution", "resume", id, "--wait")
	if got := strings.TrimSpace(s.run(t, "cat "+base+"/data/sync.log")); got != "custom" {
		t.Fatalf("sync repeated or original input lost: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(s.run(t, "cat "+base+"/data/index.log")), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected original index and resumed attempt: %q", lines)
	}
	first, second := strings.Fields(lines[0]), strings.Fields(lines[1])
	if len(first) != 3 || len(second) != 3 || first[0] != second[0] || first[1] == second[1] || second[2] != "release-123" {
		t.Fatalf("attempt identities/output handoff failed: %q", lines)
	}
	inspect := s.mustOb(t, dir, "execution", "inspect", id, "--output", "json")
	if !strings.Contains(inspect, `"state": "succeeded"`) {
		t.Fatalf("execution did not finish: %s", inspect)
	}
	if out, err := s.ob(t, dir, "execution", "resume", id, "--wait"); err == nil {
		t.Fatalf("terminal resume succeeded: %s", out)
	}
	// Interrupted work is retained independently of the host journal. A new
	// failed execution can be inspected after its runner and notifier exit.
	s.run(t, "rm -f "+base+"/data/allow")
	_, _ = s.ob(t, dir, "schedule", "run", "refresh", "--wait")
	pins := s.run(t, "/usr/bin/python3 "+root+"/schedule/execution-v1.py pins "+root)
	if strings.TrimSpace(pins) == "" {
		t.Fatal("failed execution lost its durable release reference")
	}
	// A killed activation must be inspectable and resume only its interrupted
	// step. Concurrent resume and deployment coordination must refuse live work.
	s.run(t, "touch "+base+"/data/hold")
	s.mustOb(t, dir, "schedule", "run", "refresh")
	deadline := time.Now().Add(15 * time.Second)
	for s.try(t, "test -f "+base+"/data/started") != nil {
		if time.Now().After(deadline) {
			t.Fatal("index never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
	list = s.mustOb(t, dir, "execution", "list", "--output", "json")
	if err := json.Unmarshal([]byte(list), &envelope); err != nil {
		t.Fatal(err)
	}
	id = envelope.Data.Executions[0]["id"].(string)
	if out, err := s.ob(t, dir, "execution", "resume", id); err == nil {
		t.Fatalf("resumed active work: %s", out)
	}
	if out, err := s.ob(t, dir, "schedule", "apply"); err == nil {
		t.Fatalf("exclusive execution allowed schedule apply: %s", out)
	}
	// systemd can report ESRCH for an auxiliary process that exits while the
	// cgroup is being killed. The terminal unit state below is the assertion.
	_, _ = s.output(t.Context(), "systemctl kill --kill-whom=all --signal=KILL "+unit+".service")
	deadline = time.Now().Add(10 * time.Second)
	for {
		active := strings.TrimSpace(s.run(t, "systemctl show "+unit+".service --property=ActiveState --value"))
		if active == "failed" || active == "inactive" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed execution did not stop")
		}
		time.Sleep(100 * time.Millisecond)
	}
	inspect = s.mustOb(t, dir, "execution", "inspect", id, "--output", "json")
	if !strings.Contains(inspect, `"state": "interrupted"`) {
		t.Fatalf("killed execution not interrupted: %s", inspect)
	}
	s.run(t, "rm -f "+base+"/data/hold; touch "+base+"/data/allow")
	s.mustOb(t, dir, "execution", "resume", id, "--wait")
	if got := strings.Fields(s.run(t, "cat "+base+"/data/sync.log")); len(got) != 3 {
		t.Fatalf("crash resume repeated sync: %v", got)
	}
}
