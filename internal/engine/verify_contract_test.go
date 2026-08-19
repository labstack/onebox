package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
)

func TestVerifyURLSupportsStatusAndHeaderContracts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Release", "r42")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	e := verificationTestEngine(io.Discard)
	check := app.Verification{
		URL:             srv.URL,
		StatusCodes:     []int{http.StatusCreated, http.StatusNoContent},
		RequiredHeaders: map[string]string{"content-type": "application/json", "X-Release": "r42"},
	}
	if err := e.verifyURL(context.Background(), check); err != nil {
		t.Fatalf("extended status/header check: %v", err)
	}

	check.StatusCodes = []int{http.StatusOK}
	if err := e.verifyURL(context.Background(), check); err == nil || !strings.Contains(err.Error(), "unexpected status 201") {
		t.Fatalf("unexpected status error = %v", err)
	}
}

func TestVerifyURLDoesNotFollowRedirects(t *testing.T) {
	finalHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		finalHits++
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := verificationTestEngine(io.Discard)
	check := app.Verification{
		URL:             srv.URL + "/start",
		StatusCodes:     []int{http.StatusFound},
		RequiredHeaders: map[string]string{"Location": "/final"},
	}
	if err := e.verifyURL(context.Background(), check); err != nil {
		t.Fatalf("explicit redirect contract: %v", err)
	}
	if finalHits != 0 {
		t.Fatalf("verification followed a redirect %d time(s)", finalHits)
	}

	check.StatusCodes = []int{http.StatusOK}
	if err := e.verifyURL(context.Background(), check); err == nil || !strings.Contains(err.Error(), "unexpected status 302") {
		t.Fatalf("redirect status error = %v", err)
	}
}

func TestVerifyURLSupportsDottedJSONScalarAssertions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"service":{"ready":true,"replicas":2.0,"note":null},"items":[{"name":"primary"}]}`)
	}))
	defer srv.Close()

	check := app.Verification{
		URL: srv.URL,
		JSONAssertions: []app.JSONAssertion{
			{Path: "service.ready", Equals: true},
			{Path: "service.replicas", Equals: 2},
			{Path: "service.note", Equals: nil},
			{Path: "items.0.name", Equals: "primary"},
		},
	}
	if err := verificationTestEngine(io.Discard).verifyURL(context.Background(), check); err != nil {
		t.Fatalf("JSON assertions: %v", err)
	}
}

func TestVerifyURLFailureRedactsConfiguredAndResponseValues(t *testing.T) {
	const (
		querySecret    = "query-secret"
		expectedSecret = "expected-secret"
		actualSecret   = "actual-secret"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Token", actualSecret)
		_, _ = io.WriteString(w, `{"token":"`+actualSecret+`"}`)
	}))
	defer srv.Close()

	e := verificationTestEngine(io.Discard)
	headerErr := e.verifyURL(context.Background(), app.Verification{
		URL:             srv.URL + "?token=" + querySecret,
		RequiredHeaders: map[string]string{"X-Token": expectedSecret},
	})
	assertVerificationSecretsAbsent(t, headerErr, querySecret, expectedSecret, actualSecret)

	jsonErr := e.verifyURL(context.Background(), app.Verification{
		URL: srv.URL + "?token=" + querySecret,
		JSONAssertions: []app.JSONAssertion{
			{Path: "token", Equals: expectedSecret},
		},
	})
	assertVerificationSecretsAbsent(t, jsonErr, querySecret, expectedSecret, actualSecret)

	containsErr := e.verifyURL(context.Background(), app.Verification{
		URL:      srv.URL + "?token=" + querySecret,
		Contains: expectedSecret,
	})
	assertVerificationSecretsAbsent(t, containsErr, querySecret, expectedSecret, actualSecret)
}

func TestVerifyURLBoundsBodiesUsedByAssertions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxVerificationBodyBytes+1))
	}))
	defer srv.Close()

	err := verificationTestEngine(io.Discard).verifyURL(context.Background(), app.Verification{
		URL:      srv.URL,
		Contains: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("oversized body error = %v", err)
	}
}

func TestVerifyURLDoesNotExposeInvalidJSONBody(t *testing.T) {
	const secretBody = "secret-body-is-not-json"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, secretBody)
	}))
	defer srv.Close()

	err := verificationTestEngine(io.Discard).verifyURL(context.Background(), app.Verification{
		URL:            srv.URL,
		JSONAssertions: []app.JSONAssertion{{Path: "ready", Equals: true}},
	})
	assertVerificationSecretsAbsent(t, err, secretBody)
}

func TestVerifyURLRequestErrorRedactsQuery(t *testing.T) {
	const querySecret = "query-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	err := verificationTestEngine(io.Discard).verifyURL(context.Background(), app.Verification{
		URL: url + "?token=" + querySecret,
	})
	assertVerificationSecretsAbsent(t, err, querySecret)
}

func TestVerifyURLSuccessOutputRedactsQuery(t *testing.T) {
	const querySecret = "query-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cfg := testConfig()
	cfg.Checks = app.Checks{URL: []app.URLCheck{{URL: srv.URL + "?token=" + querySecret}}}
	e := New(cfg, testProject(t), happyFake(), Options{Out: &out, Sleep: noSleep})
	if err := e.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), querySecret) {
		t.Fatalf("verification output exposed URL query: %s", out.String())
	}
}

func TestVerifyMigrationRevisionsMatchesBoundProviderEvidence(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads = map[string]app.Workload{
		"migrate": {Role: app.RoleJob, When: "pre_release", DataEffect: "migration"},
	}
	cfg.Checks = app.Checks{Migrations: []app.MigrationCheck{{
		Job: "migrate", Provider: "atlas", AppliedRevisions: []string{"r1", "r2"},
	}}}
	e := New(cfg, testProject(t), happyFake(), Options{Out: io.Discard, Sleep: noSleep})
	e.jobResults = map[string]journal.JobResultEvidence{
		"migrate": {SchemaVersion: journal.JobResultSchemaVersion, Changed: true, Provider: "atlas", AfterRevisions: []string{"r1", "r2"}, Digest: "sha256:evidence"},
	}
	if err := e.Verify(context.Background()); err != nil {
		t.Fatalf("verify matching Atlas revisions: %v", err)
	}
	e.jobResults["migrate"] = journal.JobResultEvidence{Provider: "atlas", AfterRevisions: []string{"r1"}}
	if err := e.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("mismatched revision error = %v", err)
	}
}

func verificationTestEngine(out io.Writer) *Engine {
	return New(nil, nil, nil, Options{Out: out, HTTPTimeout: 2 * time.Second})
}

func assertVerificationSecretsAbsent(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected verification failure")
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("verification error exposed secret %q: %v", secret, err)
		}
	}
}
