//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	acmetest "github.com/cert-manager/cert-manager/test/acme"

	"github.com/noderings/cert-manager-webhook-noderings/internal/nrdns"
	"github.com/noderings/cert-manager-webhook-noderings/internal/solver"
)

// TestConformance runs cert-manager's DNS-01 conformance suite against the real
// NodeRings API. It creates a throwaway zone, checks the TXT records on the
// NodeRings authoritative nameserver directly (so the zone needs no
// delegation), and deletes the zone afterwards.
//
// Run it with `make test-conformance`, which downloads the envtest binaries.
// It needs NODERINGS_TOKEN and is skipped without it.
func TestConformance(t *testing.T) {
	token := os.Getenv("NODERINGS_TOKEN")
	if token == "" {
		t.Skip("set NODERINGS_TOKEN to run the conformance suite against NodeRings")
	}
	nameserver := os.Getenv("NODERINGS_TEST_NAMESERVER")
	if nameserver == "" {
		nameserver = "ans1.nrings.io:53"
	}
	domain := os.Getenv("NODERINGS_TEST_DOMAIN")
	if domain == "" {
		domain = "example.com"
	}

	client, err := nrdns.New(nrdns.Config{
		Endpoint: os.Getenv("NODERINGS_API_URL"), Token: token,
		OrganizationID: os.Getenv("NODERINGS_ORGANIZATION_ID"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	zoneName := fmt.Sprintf("cm-conformance-%06x.%s", rand.IntN(1<<24), domain)
	zone, err := client.CreateZone(ctx, zoneName)
	if err != nil {
		t.Fatalf("creating test zone: %v", err)
	}
	t.Logf("created test zone %s (%s)", zoneName, zone.ID)
	t.Cleanup(func() {
		if err := client.DeleteZone(context.Background(), zone.ID); err != nil {
			t.Errorf("deleting test zone %s: %v", zoneName, err)
		}
	})

	// The suite applies every manifest in this directory to each test namespace.
	dir := t.TempDir()
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: noderings-token\ntype: Opaque\nstringData:\n  token: %q\n", token)
	if err := os.WriteFile(filepath.Join(dir, "secret.yaml"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"tokenSecretRef":{"name":"noderings-token","key":"token"},"ttl":60`
	if org := os.Getenv("NODERINGS_ORGANIZATION_ID"); org != "" {
		cfg += fmt.Sprintf(`,"organizationId":%q`, org)
	}
	if u := os.Getenv("NODERINGS_API_URL"); u != "" {
		cfg += fmt.Sprintf(`,"apiUrl":%q`, u)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg+"}"), 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := acmetest.NewFixture(&solver.Solver{},
		acmetest.SetResolvedZone(zoneName+"."),
		acmetest.SetManifestPath(dir),
		acmetest.SetDNSServer(nameserver),
		acmetest.SetUseAuthoritative(false),
		acmetest.SetStrict(true), // also run the "two keys on one name" test
		acmetest.SetPollInterval(2*time.Second),
		acmetest.SetPropagationLimit(2*time.Minute),
	)
	fixture.RunConformance(t)
}
