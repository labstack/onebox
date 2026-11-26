package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// testSecret is a Standard Webhooks secret whose key bytes the test knows.
var (
	testKey    = []byte("facet-test-signing-key-0123456789")
	testSecret = "whsec_" + base64.StdEncoding.EncodeToString(testKey)
)

// received is one request as the receiver saw it.
type received struct {
	id, timestamp, signature string
	body                     []byte
}

// verify is an independent Standard Webhooks verifier: it shares no code with
// the sender, so a signature that passes here is one a receiver accepts.
func verify(t *testing.T, key []byte, r received) {
	t.Helper()
	ts, err := strconv.ParseInt(r.timestamp, 10, 64)
	if err != nil {
		t.Fatalf("webhook-timestamp %q is not unix seconds: %v", r.timestamp, err)
	}
	if d := time.Since(time.Unix(ts, 0)); d < -5*time.Second || d > 5*time.Minute {
		t.Fatalf("webhook-timestamp %d is not now", ts)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(r.id + "." + r.timestamp + "." + string(r.body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(r.signature), []byte(want)) {
		t.Fatalf("webhook-signature = %q, want %q", r.signature, want)
	}
}

// receiver answers with the given status codes in turn and records requests.
func receiver(t *testing.T, statuses ...int) (*httptest.Server, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{
			id: r.Header.Get("webhook-id"), timestamp: r.Header.Get("webhook-timestamp"),
			signature: r.Header.Get("webhook-signature"), body: body,
		})
		n := len(got)
		mu.Unlock()
		if n <= len(statuses) {
			w.WriteHeader(statuses[n-1])
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received(nil), got...)
	}
}

func fastBackoff(t *testing.T) {
	t.Helper()
	old := signedBackoff
	signedBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { signedBackoff = old })
}

func signedConfig(url string) app.Notification {
	return app.Notification{Webhook: url, On: []string{"success", "failure"}, Format: "json", SigningSecretEnv: "TEST_ONEBOX_WEBHOOK_SECRET"}
}

func TestSignedSendVerifiesWithAnIndependentImplementation(t *testing.T) {
	t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", testSecret)
	srv, got := receiver(t)
	p := Payload{App: "sample", Env: "production", Host: "root@h", Verb: "deploy", DeployID: "R1", Status: "ok", Operator: "v"}
	if err := Send(t.Context(), signedConfig(srv.URL), p); err != nil {
		t.Fatal(err)
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if !strings.HasPrefix(r.id, "msg_") || r.timestamp == "" || !strings.HasPrefix(r.signature, "v1,") {
		t.Fatalf("Standard Webhooks headers missing: %+v", r)
	}
	verify(t, testKey, r)
	// The payload shape a receiver parses is unchanged by signing.
	var decoded map[string]any
	if err := json.Unmarshal(r.body, &decoded); err != nil {
		t.Fatalf("signed body is not the JSON payload: %v", err)
	}
	for _, field := range []string{"app", "env", "host", "verb", "deploy_id", "status", "operator", "ts", "text"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("payload lost %q: %s", field, r.body)
		}
	}
}

func TestUnsignedSendCarriesNoSignatureHeaders(t *testing.T) {
	srv, got := receiver(t, http.StatusServiceUnavailable)
	n := app.Notification{Webhook: srv.URL, On: []string{"failure"}, Format: "json"}
	if err := Send(t.Context(), n, Payload{Status: "fail"}); err == nil {
		t.Fatal("503 must be reported")
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("an unsigned send is one attempt, got %d", len(reqs))
	}
	if reqs[0].id != "" || reqs[0].timestamp != "" || reqs[0].signature != "" {
		t.Fatalf("unsigned send carried signing headers: %+v", reqs[0])
	}
}

func TestSignedSendRetriesWithAStableID(t *testing.T) {
	fastBackoff(t)
	t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", testSecret)
	for _, tc := range []struct {
		name     string
		statuses []int
	}{
		{"5xx", []int{http.StatusBadGateway, http.StatusServiceUnavailable}},
		{"429", []int{http.StatusTooManyRequests, http.StatusTooManyRequests}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := receiver(t, tc.statuses...)
			if err := Send(t.Context(), signedConfig(srv.URL), Payload{App: "sample", Verb: "deploy", Status: "fail", Error: "x"}); err != nil {
				t.Fatalf("third attempt succeeded, yet Send = %v", err)
			}
			reqs := got()
			if len(reqs) != 3 {
				t.Fatalf("got %d attempts, want 3", len(reqs))
			}
			for _, r := range reqs {
				if r.id != reqs[0].id {
					t.Fatalf("webhook-id changed across retries: %q then %q", reqs[0].id, r.id)
				}
				// Each attempt is signed afresh over its own timestamp.
				verify(t, testKey, r)
			}
		})
	}
}

func TestSignedSendGivesUpAfterThreeAttempts(t *testing.T) {
	fastBackoff(t)
	t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", testSecret)
	srv, got := receiver(t, 500, 500, 500, 500)
	err := Send(t.Context(), signedConfig(srv.URL), Payload{Status: "fail"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Send = %v, want the last 500", err)
	}
	if n := len(got()); n != SignedAttempts {
		t.Fatalf("got %d attempts, want %d", n, SignedAttempts)
	}
}

func TestSignedSendDoesNotRetryAClientError(t *testing.T) {
	fastBackoff(t)
	t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", testSecret)
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		srv, got := receiver(t, status, status, status)
		err := Send(t.Context(), signedConfig(srv.URL), Payload{Status: "fail"})
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Fatalf("Send = %v, want %d reported", err, status)
		}
		if n := len(got()); n != 1 {
			t.Fatalf("%d: got %d attempts; a refused request is not retried", status, n)
		}
	}
}

func TestSignedSendRetriesANetworkErrorWithinTheBudget(t *testing.T) {
	fastBackoff(t)
	t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", testSecret)
	start := time.Now()
	err := Send(t.Context(), signedConfig("http://127.0.0.1:1"), Payload{Status: "fail"})
	if err == nil {
		t.Fatal("a dead webhook must surface an error for the warn log")
	}
	if elapsed := time.Since(start); elapsed > SignedBudget+time.Second {
		t.Fatalf("signed send took %s, beyond its %s budget", elapsed, SignedBudget)
	}
	if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(testKey)) {
		t.Fatalf("error carries the secret: %v", err)
	}
}

func TestSignedSendWithoutAUsableSecretIsAWarningAndSendsNothing(t *testing.T) {
	for name, value := range map[string]string{
		"unset":     "",
		"no prefix": "c2VjcmV0LWNhbmFyeS12YWx1ZQ==",
		"not b64":   "whsec_secret-canary-value!!",
		"empty key": "whsec_",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_ONEBOX_WEBHOOK_SECRET", value)
			srv, got := receiver(t)
			err := Send(t.Context(), signedConfig(srv.URL), Payload{Status: "fail"})
			if err == nil {
				t.Fatal("a missing or malformed secret must be reported")
			}
			if !strings.Contains(err.Error(), "TEST_ONEBOX_WEBHOOK_SECRET") {
				t.Fatalf("error must name the variable: %v", err)
			}
			if value != "" && value != "whsec_" && strings.Contains(err.Error(), strings.TrimPrefix(value, "whsec_")) {
				t.Fatalf("error carries the secret value: %v", err)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatalf("error carries the secret value: %v", err)
			}
			if n := len(got()); n != 0 {
				t.Fatalf("sent %d unsigned request(s) to a receiver that expects signatures", n)
			}
		})
	}
}

func TestSignatureMatchesTheStandardWebhooksExample(t *testing.T) {
	// The worked example from the Standard Webhooks specification's reference
	// implementations.
	key, err := ParseSigningSecret("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatal(err)
	}
	got := Signature(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("Signature = %q, want %q", got, want)
	}
}
