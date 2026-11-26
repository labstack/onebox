package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	return runSignedNotifierWith(t, codes, secret, true)
}

// signedRun is everything one notifier execution exposed: the rendered
// script, each curl attempt, the combined log, and every argument list the
// host's openssl and curl were started with — what ps and /proc/<pid>/cmdline
// would show.
func runSignedNotifierWith(t *testing.T, codes []string, secret string, writePads bool) (string, []signedAttempt, string) {
	script, attempts, log, _ := runSignedNotifierArgv(t, codes, secret, writePads)
	return script, attempts, log
}

func runSignedNotifierArgv(t *testing.T, codes []string, secret string, writePads bool) (string, []signedAttempt, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	realOpenSSL, err := exec.LookPath("openssl")
	if err != nil {
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
	script, pads, err := e.scheduleNotifierFiles(app.ScheduledJob{Name: "nightly", Notify: []string{"failure"}})
	if err != nil {
		t.Fatal(err)
	}
	// Run it the way systemd does, `/bin/sh <path>`, so $0 finds the pads.
	notifyPath := filepath.Join(base, "onebox-job-nightly.notify")
	if err := os.WriteFile(notifyPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if writePads {
		for suffix, pad := range pads {
			if err := os.WriteFile(notifyPath+suffix, pad, 0o600); err != nil {
				t.Fatal(err)
			}
		}
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
	argv := filepath.Join(bin, "argv")
	openssl := "#!/bin/sh\nprintf 'openssl %s\\n' \"$*\" >>" + argv + "\nexec " + realOpenSSL + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "openssl"), []byte(openssl), 0o755); err != nil {
		t.Fatal(err)
	}
	curl := "#!/bin/sh\n" +
		"printf 'curl %s\\n' \"$*\" >>" + argv + "\n" +
		"n=$(cat " + bin + "/count 2>/dev/null || echo 0); n=$((n + 1)); echo \"$n\" >" + bin + "/count\n" +
		"printf '%s\\n' \"$@\" >" + bin + "/args.$n\n" +
		"code=$(sed -n \"${n}p\" " + bin + "/codes); [ -n \"$code\" ] || code=200\n" +
		"printf '%s' \"$code\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), "sh", notifyPath)
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
	argvLog, _ := os.ReadFile(argv)
	return script, attempts, string(stderr) + out.String(), string(argvLog)
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

// The key, and the pads derived from it, are never a command argument: not in
// the rendered notifier, where every string becomes some process's argv, and
// not in the argument lists the host actually started openssl and curl with.
func TestScheduledJobSigningKeyIsNeverACommandArgument(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString(scheduleSigningKey)
	script, attempts, _, argv := runSignedNotifierArgv(t, []string{"200"}, secret, true)
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(attempts))
	}
	verifyScheduleSignature(t, attempts[0])
	inner, outer := hmacPads(scheduleSigningKey)
	forbidden := map[string]string{
		"secret":            secret,
		"base64 key":        base64.StdEncoding.EncodeToString(scheduleSigningKey),
		"hex key":           hex.EncodeToString(scheduleSigningKey),
		"raw key":           string(scheduleSigningKey),
		"hex inner pad":     hex.EncodeToString(inner),
		"hex outer pad":     hex.EncodeToString(outer),
		"base64 inner pad":  base64.StdEncoding.EncodeToString(inner),
		"openssl key flag":  "hexkey",
		"openssl macopt":    "macopt",
		"openssl hmac flag": "-hmac",
	}
	for what, value := range forbidden {
		if strings.Contains(script, value) {
			t.Errorf("the rendered notifier contains the %s:\n%s", what, script)
		}
		if strings.Contains(argv, value) {
			t.Errorf("a host command was started with the %s in its arguments:\n%s", what, argv)
		}
	}
	if !strings.Contains(argv, "openssl dgst -sha256 -binary") {
		t.Fatalf("the argv log did not capture the signing commands:\n%s", argv)
	}
}

func TestHMACPadsMatchHMAC(t *testing.T) {
	for _, key := range [][]byte{scheduleSigningKey, bytes.Repeat([]byte{7}, 64), bytes.Repeat([]byte{9}, 100)} {
		inner, outer := hmacPads(key)
		message := []byte("msg_1.1614265330.{}")
		h := sha256.Sum256(append(append([]byte{}, inner...), message...))
		got := sha256.Sum256(append(append([]byte{}, outer...), h[:]...))
		mac := hmac.New(sha256.New, key)
		mac.Write(message)
		if !hmac.Equal(got[:], mac.Sum(nil)) {
			t.Fatalf("pads for a %d-byte key do not reproduce HMAC-SHA256", len(key))
		}
	}
}

func TestScheduledJobWithMissingPadsSendsNothing(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString(scheduleSigningKey)
	_, attempts, log := runSignedNotifierWith(t, []string{"200"}, secret, false)
	if len(attempts) != 0 {
		t.Fatalf("sent %d request(s) without the pads to sign them", len(attempts))
	}
	if !strings.Contains(log, "signing pads are missing") {
		t.Fatalf("the host must log why it sent nothing:\n%s", log)
	}
}

func TestScheduleApplyWritesPadsBesideTheNotifierAndClearsStaleOnes(t *testing.T) {
	t.Setenv("TEST_SCHEDULE_WEBHOOK_SECRET", "whsec_"+base64.StdEncoding.EncodeToString(scheduleSigningKey))
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, DeploymentPhase: "none", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "45m", Notify: []string{"failure"}},
	}
	cfg.Notifications = map[string]app.Notification{
		"facet": {Webhook: "https://facet.example.invalid/hooks/onebox", On: []string{"failure"}, Format: "json", SigningSecretEnv: "TEST_SCHEDULE_WEBHOOK_SECRET"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "current/compose.yaml"), strings.Contains(cmd, "systemd-analyze calendar"), strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.ScheduleApply(context.Background(), "R9-schedule-apply"); err != nil {
		t.Fatalf("schedule apply: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	notify := "/etc/systemd/system/onebox-job-nightly.notify"
	for _, want := range []string{
		"rm -f '" + notify + "'.*.ipad '" + notify + "'.*.opad",
		"cat > '" + notify + ".0.ipad.onebox-tmp'",
		"cat > '" + notify + ".0.opad.onebox-tmp'",
	} {
		if !strings.Contains(seq, want) {
			t.Errorf("schedule apply is missing %q:\n%s", want, seq)
		}
	}
	inner, outer := hmacPads(scheduleSigningKey)
	var sawInner, sawOuter bool
	for _, in := range f.Inputs {
		sawInner = sawInner || in == string(inner)
		sawOuter = sawOuter || in == string(outer)
	}
	if !sawInner || !sawOuter {
		t.Fatal("the pads were not written to the host")
	}
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, string(inner)) || strings.Contains(cmd, hex.EncodeToString(inner)) || strings.Contains(cmd, hex.EncodeToString(scheduleSigningKey)) {
			t.Fatalf("a host command carries key material: %q", cmd)
		}
	}
}
