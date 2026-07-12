package onebox

import (
	"testing"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

func TestStatusDigestIgnoresCaptureTimeAndCertificateCountdown(t *testing.T) {
	expires := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	first := engine.StatusSnapshot{
		App:        "demo",
		Host:       "example.invalid",
		CapturedAt: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		Complete:   true,
		Proxy: &engine.StatusProxy{
			Managed:  true,
			Complete: true,
			Certificates: []engine.StatusCertificate{{
				Domain: "example.invalid", NotAfter: expires, DaysRemaining: 22, RenewalOverdue: false,
			}},
		},
	}
	second := first
	second.CapturedAt = first.CapturedAt.Add(48 * time.Hour)
	proxyCopy := *first.Proxy
	proxyCopy.Certificates = append([]engine.StatusCertificate(nil), first.Proxy.Certificates...)
	proxyCopy.Certificates[0].DaysRemaining = 20
	second.Proxy = &proxyCopy

	firstDigest, err := statusDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := statusDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("presentation time changed status identity: %q != %q", firstDigest, secondDigest)
	}

	proxyCopy.Certificates[0].RenewalOverdue = true
	operationalDigest, err := statusDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if operationalDigest == firstDigest {
		t.Fatal("renewal threshold crossing must change status identity")
	}
}
