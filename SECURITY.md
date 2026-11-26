# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Email **partners@noderings.com** with:

- a description of the issue and its impact
- steps to reproduce, or a proof of concept
- the chart and image version and the cert-manager version

We acknowledge reports within three working days and keep you updated until the issue is fixed. Credit is given in the release notes unless you ask us not to.

## Supported versions

Security fixes are made for the latest minor release.

## Handling credentials

Issuers name a Secret holding the token in `tokenSecretRef`; the solver reads it with its own service account, which the Helm chart only allows to read the Secret names listed in `secretAccess.secretNames` (default `noderings-token`) in the namespaces you allow. It never logs the token. Give the service account only the DNS permissions it needs.
