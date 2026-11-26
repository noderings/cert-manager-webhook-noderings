package solver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	whapi "github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/noderings/cert-manager-webhook-noderings/internal/nrdns"
	"github.com/noderings/cert-manager-webhook-noderings/internal/nrdns/nrdnstest"
)

func newSolver(t *testing.T, zones ...string) (*nrdnstest.API, *Solver) {
	t.Helper()
	fake := nrdnstest.New()
	t.Cleanup(fake.Close)
	for _, z := range zones {
		fake.AddZone(z)
	}
	s := &Solver{
		ReadSecret: func(_ context.Context, ns, name, key string) (string, error) {
			if ns == "cert-manager" && name == "noderings-token" && key == "token" {
				return nrdnstest.Token, nil
			}
			return "", errors.New("secret not found")
		},
		NewClient: func(c nrdns.Config) (*nrdns.Client, error) {
			c.Endpoint = fake.URL
			c.MinBackoff = 1
			return nrdns.New(c)
		},
		Getenv: func(string) string { return "" },
	}
	return fake, s
}

func challenge(fqdn, zone, key string) *whapi.ChallengeRequest {
	return &whapi.ChallengeRequest{
		UID: "uid", ResolvedFQDN: fqdn, ResolvedZone: zone, Key: key, ResourceNamespace: "cert-manager",
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"tokenSecretRef":{"name":"noderings-token","key":"token"}}`)},
	}
}

func txt(fake *nrdnstest.API, zone, name string) []string {
	for _, r := range fake.Records(zone) {
		if r.Name == name && r.Type == "TXT" {
			return r.RData
		}
	}
	return nil
}

func TestPresentAndCleanUpKeepOtherValues(t *testing.T) {
	fake, s := newSolver(t, "example.com")
	apex := challenge("_acme-challenge.example.com.", "example.com.", "key-apex")
	wild := challenge("_acme-challenge.example.com.", "example.com.", "key-wildcard")

	for _, ch := range []*whapi.ChallengeRequest{apex, wild, apex} { // the repeat must be a no-op
		if err := s.Present(ch); err != nil {
			t.Fatal(err)
		}
	}
	got := txt(fake, "example.com", "_acme-challenge")
	if !slices.Equal(got, []string{`"key-apex"`, `"key-wildcard"`}) {
		t.Fatalf("both keys present once: %v", got)
	}
	recs := fake.Records("example.com")
	if recs[0].TTL != 60 {
		t.Errorf("default TTL 60, got %d", recs[0].TTL)
	}

	if err := s.CleanUp(apex); err != nil {
		t.Fatal(err)
	}
	if got := txt(fake, "example.com", "_acme-challenge"); !slices.Equal(got, []string{`"key-wildcard"`}) {
		t.Fatalf("cleanup removes only its own key: %v", got)
	}
	for range 2 { // the second cleanup must be a no-op
		if err := s.CleanUp(wild); err != nil {
			t.Fatal(err)
		}
	}
	if got := txt(fake, "example.com", "_acme-challenge"); got != nil {
		t.Fatalf("record set deleted with its last key: %v", got)
	}
}

func TestConcurrentPresentsOnOneName(t *testing.T) {
	fake, s := newSolver(t, "example.com")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := s.Present(challenge("_acme-challenge.example.com.", "example.com.", "k"+string(rune('a'+i)))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := txt(fake, "example.com", "_acme-challenge"); len(got) != 8 {
		t.Fatalf("all 8 keys kept: %v", got)
	}
}

func TestZoneSelection(t *testing.T) {
	fake, s := newSolver(t, "example.com", "dev.example.com")
	// cert-manager resolved the parent zone (the child is not delegated yet), but
	// NodeRings hosts the child: the exact match wins.
	if err := s.Present(challenge("_acme-challenge.app.dev.example.com.", "example.com.", "k1")); err != nil {
		t.Fatal(err)
	}
	if got := txt(fake, "example.com", "_acme-challenge.app.dev"); got == nil {
		t.Fatalf("expected record in example.com: %v", fake.Records("example.com"))
	}
	// A resolved zone NodeRings does not host falls back to the longest matching zone.
	if err := s.Present(challenge("_acme-challenge.api.dev.example.com.", "other.net.", "k2")); err != nil {
		t.Fatal(err)
	}
	if got := txt(fake, "dev.example.com", "_acme-challenge.api"); got == nil {
		t.Fatalf("expected record in dev.example.com: %v", fake.Records("dev.example.com"))
	}
	err := s.Present(challenge("_acme-challenge.elsewhere.org.", "elsewhere.org.", "k3"))
	if err == nil || !strings.Contains(err.Error(), "no NodeRings DNS zone hosts elsewhere") && !strings.Contains(err.Error(), "_acme-challenge.elsewhere.org") {
		t.Fatalf("want a clear error for an unknown zone, got %v", err)
	}
}

func TestCredentials(t *testing.T) {
	_, s := newSolver(t, "example.com")
	ch := challenge("_acme-challenge.example.com.", "example.com.", "k")

	ch.Config = &apiextensionsv1.JSON{Raw: []byte(`{"tokenSecretRef":{"name":"missing"}}`)}
	if err := s.Present(ch); err == nil || !strings.Contains(err.Error(), "secret not found") {
		t.Errorf("missing secret: %v", err)
	}

	ch.Config = nil
	if err := s.Present(ch); err == nil || !strings.Contains(err.Error(), "tokenSecretRef") {
		t.Errorf("no config and no ambient credentials: %v", err)
	}

	ch.AllowAmbientCredentials = true
	s.Getenv = func(k string) string {
		if k == "NODERINGS_TOKEN" {
			return nrdnstest.Token
		}
		return ""
	}
	if err := s.Present(ch); err != nil {
		t.Errorf("ambient token for a ClusterIssuer: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig(&apiextensionsv1.JSON{Raw: []byte(`{"tokenSecretRef":{"name":"t","key":"k"},"ttl":120,"organizationId":"org"}`)})
	if err != nil || cfg.TTL != 120 || cfg.OrganizationID != "org" || cfg.TokenSecretRef.Key != "k" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if _, err := LoadConfig(&apiextensionsv1.JSON{Raw: []byte(`{"tokenSecret":"typo"}`)}); err == nil {
		t.Error("unknown fields are rejected")
	}
	if _, err := LoadConfig(&apiextensionsv1.JSON{Raw: []byte(`{"tokenSecretRef":{"key":"token"}}`)}); err == nil {
		t.Error("a secret ref needs a name")
	}
	if cfg, err := LoadConfig(nil); err != nil || cfg.TTL != 60 {
		t.Errorf("nil config: %+v %v", cfg, err)
	}
}
