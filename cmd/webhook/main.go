// Command webhook runs the NodeRings ACME DNS-01 solver for cert-manager as a
// Kubernetes API extension server.
package main

import (
	"log/slog"
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"

	"github.com/noderings/cert-manager-webhook-noderings/internal/solver"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		groupName = "acme.noderings.com"
	}
	solver.Version = version
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("solver", solver.Name)
	cmd.RunWebhookServer(groupName, &solver.Solver{Log: log})
}
