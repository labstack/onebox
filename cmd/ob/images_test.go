package main

import "testing"

func TestParseImagesValidatesReferencesWithoutNormalizing(t *testing.T) {
	images, err := parseImages([]string{"web=registry.example.com:5000/acme/app:1.2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := images["web"]; got != "registry.example.com:5000/acme/app:1.2" {
		t.Fatalf("image reference was rewritten: %q", got)
	}
}

func TestParseImagesRejectsInvalidReference(t *testing.T) {
	if _, err := parseImages([]string{"web=ghcr.io/Acme/app:1.2"}); err == nil {
		t.Fatal("invalid image reference was accepted")
	}
}
