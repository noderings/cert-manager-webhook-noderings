// Package solver is a cert-manager ACME DNS-01 solver for NodeRings DNS.
//
// Present adds the challenge key to the _acme-challenge TXT record set and
// CleanUp removes it again. Other values in the record set are kept, because a
// certificate for example.com and *.example.com puts two keys on the same name.
package solver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	whapi "github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/noderings/cert-manager-webhook-noderings/internal/nrdns"
)

// Name is the solver name used in an Issuer's webhook config.
const Name = "noderings"

// Config is the solver configuration from the Issuer:
//
//	webhook:
//	  groupName: acme.noderings.com
//	  solverName: noderings
//	  config:
//	    tokenSecretRef: {name: noderings-token, key: token}
type Config struct {
	// TokenSecretRef names the Secret holding a NodeRings service account
	// token. An Issuer reads it from its own namespace; a ClusterIssuer from
	// cert-manager's cluster resource namespace.
	TokenSecretRef *SecretKeySelector `json:"tokenSecretRef,omitempty"`
	// OrganizationID picks the organization for tokens that reach several.
	OrganizationID string `json:"organizationId,omitempty"`
	// APIURL overrides the NodeRings API endpoint.
	APIURL string `json:"apiUrl,omitempty"`
	// TTL of the challenge record in seconds. Default 60.
	TTL int64 `json:"ttl,omitempty"`
}

// SecretKeySelector selects a key of a Secret.
type SecretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// SecretReader returns one key of a Secret.
type SecretReader func(ctx context.Context, namespace, name, key string) (string, error)

// Solver implements webhook.Solver for NodeRings DNS.
type Solver struct {
	// ReadSecret reads Secrets. Initialize sets it from the Kubernetes client
	// (again on every call); tests that skip Initialize set it directly.
	ReadSecret SecretReader
	// NewClient builds an API client. Nil uses nrdns.New.
	NewClient func(nrdns.Config) (*nrdns.Client, error)
	// Getenv reads the environment, for ambient credentials. Nil uses os.Getenv.
	Getenv func(string) string
	Log    *slog.Logger

	locks sync.Map // record set key -> *sync.Mutex
}

// Name implements webhook.Solver.
func (s *Solver) Name() string { return Name }

// Initialize implements webhook.Solver.
func (s *Solver) Initialize(cfg *rest.Config, _ <-chan struct{}) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating Kubernetes client: %w", err)
	}
	s.ReadSecret = func(ctx context.Context, namespace, name, key string) (string, error) {
		sec, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("reading Secret %s/%s: %w", namespace, name, err)
		}
		v, ok := sec.Data[key]
		if !ok {
			return "", fmt.Errorf("secret %s/%s has no key %q", namespace, name, key)
		}
		return strings.TrimSpace(string(v)), nil
	}
	return nil
}

// Present implements webhook.Solver: it adds the challenge key to the TXT record set.
func (s *Solver) Present(ch *whapi.ChallengeRequest) error {
	return s.change(ch, true)
}

// CleanUp implements webhook.Solver: it removes the challenge key from the TXT record set.
func (s *Solver) CleanUp(ch *whapi.ChallengeRequest) error {
	return s.change(ch, false)
}

func (s *Solver) change(ch *whapi.ChallengeRequest, present bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := s.logger().With("fqdn", ch.ResolvedFQDN, "zone", ch.ResolvedZone, "namespace", ch.ResourceNamespace, "uid", ch.UID)

	cfg, err := LoadConfig(ch.Config)
	if err != nil {
		return err
	}
	client, err := s.client(ctx, cfg, ch)
	if err != nil {
		return err
	}
	zones, err := client.ListZones(ctx)
	if err != nil {
		return fmt.Errorf("listing NodeRings DNS zones: %w", err)
	}
	zone, ok := pickZone(zones, ch.ResolvedZone, ch.ResolvedFQDN)
	if !ok {
		return fmt.Errorf("no NodeRings DNS zone hosts %s; create the zone in NodeRings first", strings.TrimSuffix(ch.ResolvedFQDN, "."))
	}
	name := nrdns.RelativeName(ch.ResolvedFQDN, zone.Name)
	value := nrdns.QuoteTXT(ch.Key)

	// Serialize changes to one record set: an apex and a wildcard certificate
	// present two keys on the same name at the same time.
	mu, _ := s.locks.LoadOrStore(zone.ID+"|"+name, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	current, ttl, err := txtValues(ctx, client, zone.ID, name)
	if err != nil {
		return err
	}
	var next []string
	if present {
		if slices.Contains(current, value) {
			log.Info("challenge record already present")
			return nil
		}
		next = append(slices.Clone(current), value)
		ttl = cfg.TTL
	} else {
		next = slices.DeleteFunc(slices.Clone(current), func(v string) bool { return v == value })
		if len(next) == len(current) {
			log.Info("challenge record already removed")
			return nil
		}
	}

	if len(next) == 0 {
		if err := client.DeleteRecord(ctx, zone.ID, name, "TXT"); err != nil {
			return fmt.Errorf("deleting TXT %s in %s: %w", name, nrdns.NormalizeName(zone.Name), err)
		}
		log.Info("removed challenge record set")
		return nil
	}
	if ttl <= 0 {
		ttl = cfg.TTL
	}
	if err := client.UpsertRecord(ctx, zone.ID, nrdns.Record{Name: name, Type: "TXT", TTL: nrdns.Int64(ttl), RData: next}); err != nil {
		return fmt.Errorf("writing TXT %s in %s: %w", name, nrdns.NormalizeName(zone.Name), err)
	}
	if present {
		log.Info("presented challenge record", "values", len(next))
	} else {
		log.Info("removed challenge record", "valuesLeft", len(next))
	}
	return nil
}

func (s *Solver) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Solver) client(ctx context.Context, cfg Config, ch *whapi.ChallengeRequest) (*nrdns.Client, error) {
	getenv := s.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	var token string
	switch {
	case cfg.TokenSecretRef != nil:
		if s.ReadSecret == nil {
			return nil, errors.New("solver not initialized: no Kubernetes client")
		}
		key := cfg.TokenSecretRef.Key
		if key == "" {
			key = "token"
		}
		t, err := s.ReadSecret(ctx, ch.ResourceNamespace, cfg.TokenSecretRef.Name, key)
		if err != nil {
			return nil, err
		}
		token = t
	case ch.AllowAmbientCredentials:
		token = strings.TrimSpace(getenv("NODERINGS_TOKEN"))
		if token == "" {
			return nil, errors.New("no tokenSecretRef in the issuer config and NODERINGS_TOKEN is not set in the webhook")
		}
	default:
		return nil, errors.New("set tokenSecretRef in the issuer's webhook config (ambient credentials are only used by ClusterIssuers)")
	}
	org := cfg.OrganizationID
	if org == "" && ch.AllowAmbientCredentials {
		org = getenv("NODERINGS_ORGANIZATION_ID")
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = getenv("NODERINGS_API_URL")
	}
	newClient := s.NewClient
	if newClient == nil {
		newClient = nrdns.New
	}
	return newClient(nrdns.Config{
		Endpoint: apiURL, Token: token, OrganizationID: org,
		UserAgent: "cert-manager-webhook-noderings/" + Version, Logger: s.logger(),
	})
}

// Version is set at build time.
var Version = "dev"

// LoadConfig decodes and checks the issuer's webhook config.
func LoadConfig(raw *apiextensionsv1.JSON) (Config, error) {
	cfg := Config{TTL: 60}
	if raw == nil {
		return cfg, nil
	}
	b := raw.Raw
	if len(b) == 0 || string(b) == "null" {
		return cfg, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decoding the NodeRings webhook config: %w", err)
	}
	if cfg.TokenSecretRef != nil && cfg.TokenSecretRef.Name == "" {
		return cfg, errors.New("tokenSecretRef.name is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 60
	}
	return cfg, nil
}

// pickZone finds the NodeRings zone cert-manager resolved, or failing that the
// longest zone that contains the challenge name (for zones cert-manager could
// not see because they are not delegated yet).
func pickZone(zones []nrdns.Zone, resolvedZone, fqdn string) (nrdns.Zone, bool) {
	for _, z := range zones {
		if nrdns.NormalizeName(z.Name) == nrdns.NormalizeName(resolvedZone) {
			return z, true
		}
	}
	return nrdns.FindZone(zones, fqdn)
}

func txtValues(ctx context.Context, c *nrdns.Client, zoneID, name string) ([]string, int64, error) {
	filter := name
	if name == "@" {
		filter = ""
	}
	recs, err := c.ListRecords(ctx, zoneID, filter, "TXT")
	if err != nil {
		return nil, 0, fmt.Errorf("reading TXT records: %w", err)
	}
	for _, r := range recs {
		if strings.EqualFold(r.Name, name) && strings.EqualFold(r.Type, "TXT") {
			return r.RData, int64(r.TTL), nil
		}
	}
	return nil, 0, nil
}
