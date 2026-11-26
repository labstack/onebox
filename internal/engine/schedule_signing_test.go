package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

var scheduleSigningKey = []byte("schedule-signing-key-for-tests-0001")

// signedAttempt is one curl invocation by the host notifier.
type signedAttempt struct {
	headers map[string]string
	body    string
}

// runSignedNotifier executes the generated notifier for a failed run with a
// curl stub that answers each attempt with the next of codes, and real
// openssl, which is what signs on the host.
func runSignedNotifier(t *testing.T, codes []string, secret string) (string, []signedAttempt, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is what signs on the host, and it is not installed here")
	}
	t.Setenv("TEST_SCHEDULE_WEBHOOK_SECRET", secret)
	base := t.TempDir()
	cfg := testConfig()
	cfg.BasePath = base
	cfg.Notifications = map[string]app.Notification{
		"facet": {Webhook: "https://facet.example.invalid/hooks/onebox", On: []string{"failure"}, Format: "json", SigningSecretEnv: "TEST_SCHEDULE_WEBHOOK_SECRET"},
	}
	out := &bytes.Buffer{}
	e := New(cfg, testProject(t), &transport.Fake{TargetName: "root@example.internal"}, Options{Environment: "production", Out: out, Sleep: noSleep})
	script, err := e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"failure"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "app", "schedule"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "logger"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codes"), []byte(strings.Join(codes, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	curl := "#!/bin/sh\n" +
		"n=$(cat " + bin + "/count 2>/dev/null || echo 0); n=$((n + 1)); echo \"$n\" >" + bin + "/count\n" +
		"printf '%s\\n' \"$@\" >" + bin + "/args.$n\n" +
		"code=$(sed -n \"${n}p\" " + bin + "/codes); [ -n \"$code\" ] || code=200\n" +
		"printf '%s' \"$code\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), "sh", "-s")
	command.Stdin = strings.NewReader(script)
	command.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + base,
		"SERVICE_RESULT=exit-code", "EXIT_STATUS=1", "INVOCATION_ID=0123456789abcdef0123456789abcdef",
	}
	stderr, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("notifier exited non-zero: %v\n%s", err, stderr)
	}
	var attempts []signedAttempt
	for n := 1; ; n++ {
		raw, err := os.ReadFile(filepath.Join(bin, "args."+strconv.Itoa(n)))
		if err != nil {
			break
		}
		args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		a := signedAttempt{headers: map[string]string{}}
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "--header":
				k, v, _ := strings.Cut(args[i+1], ": ")
				a.headers[strings.ToLower(k)] = v
			case "--data-binary":
				a.body = args[i+1]
			}
		}
		attempts = append(attempts, a)
	}
	return script, attempts, string(stderr) + out.String()
}

func verifyScheduleSignature(t *testing.T, a signedAttempt) {
	t.Helper()
	id, ts := a.headers["webhook-id"], a.headers["webhook-timestamp"]
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(seconds, 0)) > time.Minute {
		t.Fatalf("webhook-timestamp %q is not the current unix time", ts)
	}
	mac := hmac.New(sha256.New, scheduleSigningKey)
	mac.Write([]byte(id + "." + ts + "." + a.body))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if a.headers["webhook-signature"] != want {
		t.Fatalf("webhook-signature = %q, want %q over %q", a.headers["webhook-signature"], want, a.body)
	}
}

func TestScheduledJobSignsItsNotificationOnTheHost(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString(scheduleSigningKey)
	_, attempts, _ := runSignedNotifier(t, []string{"200"}, secret)
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(attempts))
	}
	a := attempts[0]
	if a.headers["webhook-id"] != "msg_0123456789abcdef0123456789abcdef_0" {
		t.Fatalf("webhook-id = %q, want the run's invocation id", a.headers["webhook-id"])
	}
	verifyScheduleSignature(t, a)
	var payload map[string]any
	if err := json.Unmarshal([]byte(a.body), &payload); err != nil {
		t.Fatalf("signed body is not JSON: %v\n%s", err, a.body)
	}
	if payload["deploy_id"] != "0123456789abcdef0123456789abcdef" || payload["status"] != "fail" {
		t.Fatalf("host-filled fields are wrong: %v", payload)
	}
}

func TestScheduledJobRetriesASignedSendWithTheSameID(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString(scheduleSigningKey)
	_, attempts, _ := runSignedNotifier(t, []string{"503", "000", "204"}, secret)
	if len(attempts) != 3 {
		t.Fatalf("got %d attempts, want 3", len(attempts))
	}
	for _, a := range attempts {
		if a.headers["webhook-id"] != attempts[0].headers["webhook-id"] {
			t.Fatalf("webhook-id changed across retries")
		}
		verifyScheduleSignature(t, a)
	}
}

func TestScheduledJobDoesNotRetryARefusedSignedSend(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString(scheduleSigningKey)
	_, attempts, log := runSignedNotifier(t, []string{"401", "200"}, secret)
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts; a 401 is not retried", len(attempts))
	}
	if !strings.Contains(log, "onebox: notification facet failed") {
		t.Fatalf("a refused send must be logged:\n%s", log)
	}
}

func TestScheduledJobWithoutASigningSecretSendsNothingUnsigned(t *testing.T) {
	script, attempts, log := runSignedNotifier(t, nil, "")
	if len(attempts) != 0 {
		t.Fatalf("sent %d unsigned request(s) to a receiver that verifies signatures", len(attempts))
	}
	if !strings.Contains(log, "TEST_SCHEDULE_WEBHOOK_SECRET") || !strings.Contains(log, "will not be sent from the host") {
		t.Fatalf("the deploy must warn that the notification is off:\n%s", log)
	}
	if !strings.Contains(log, "signing secret was unavailable") {
		t.Fatalf("the host must log why it sent nothing:\n%s", log)
	}
	if strings.Contains(script, "openssl") {
		t.Fatalf("a notifier with no key must not try to sign:\n%s", script)
	}
}

func TestSignedScheduleExtendsTheStopTimeout(t *testing.T) {
	job := app.ScheduledJob{Name: "nightly", Timeout: "45m", ShutdownGrace: 30 * time.Second}
	plain := scheduleServiceUnit("sample", job, "/r", "/n", false)
	signed := scheduleServiceUnit("sample", job, "/r", "/n", true)
	if !strings.Contains(plain, "TimeoutStopSec=40s") || !strings.Contains(signed, "TimeoutStopSec=50s") {
		t.Fatalf("stop timeouts:\n%s\n%s", plain, signed)
	}
}
