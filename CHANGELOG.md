# Changelog

All notable changes to this project are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.1](https://github.com/noderings/cert-manager-webhook-noderings/compare/v0.1.0...v0.1.1) (2026-11-28)


### Documentation

* link the official Go SDK in the README ([#4](https://github.com/noderings/cert-manager-webhook-noderings/issues/4)) ([754859c](https://github.com/noderings/cert-manager-webhook-noderings/commit/754859cbcff2d30d7f9ce0c182c77cbab44e397e))
* use the security.txt contact in SECURITY.md ([#6](https://github.com/noderings/cert-manager-webhook-noderings/issues/6)) ([4282f2e](https://github.com/noderings/cert-manager-webhook-noderings/commit/4282f2ee2b183aadae756f8756b6987446ba94a3))

## [Unreleased]

## [0.1.0] - 2026-11-26

### Added

- ACME DNS-01 solver for cert-manager (group `acme.noderings.com`, solver `noderings`).
  It adds and removes only its own value, so apex and wildcard challenges on the same name
  work together.
- Token from a Secret named in the issuer (`tokenSecretRef`) or, for ClusterIssuers, from
  the solver's own environment.
- Helm chart with least-privilege access to the token Secret, published to
  `oci://ghcr.io/noderings/charts`.
- Multi-arch image `ghcr.io/noderings/cert-manager-webhook-noderings` (linux/amd64, linux/arm64).
- cert-manager's DNS-01 conformance suite (strict mode) and an end-to-end test in kind
  with the Pebble ACME server, both against the NodeRings API.

[Unreleased]: https://github.com/noderings/cert-manager-webhook-noderings/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/noderings/cert-manager-webhook-noderings/releases/tag/v0.1.0
