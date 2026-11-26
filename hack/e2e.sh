#!/usr/bin/env bash
# End-to-end test: cert-manager issues a certificate through this webhook in a
# kind cluster, with the real NodeRings DNS API.
#
#   NODERINGS_TOKEN=... hack/e2e.sh
#
# The ACME server is Pebble (Let's Encrypt's test server) running in the
# cluster. Pebble and cert-manager both look up the challenge records on the
# NodeRings authoritative nameserver directly, so the throwaway test zone
# (<random>.example.com by default) needs no delegation. The certificate covers
# the zone apex and its wildcard, which puts two challenge keys on the same
# _acme-challenge name. The zone and the kind cluster are deleted at the end
# (KEEP_CLUSTER=1 keeps the cluster). It uses its own kubeconfig file and never
# touches ~/.kube/config.
set -euo pipefail

: "${NODERINGS_TOKEN:?set NODERINGS_TOKEN}"
API="${NODERINGS_API_URL:-https://api.noderings.com}"
CLUSTER="${CLUSTER:-noderings-cm-e2e}"
IMAGE="${IMAGE:-ghcr.io/noderings/cert-manager-webhook-noderings:e2e}"
NAMESERVER="${NAMESERVER:-ans1.nrings.io}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.21.3}"
PEBBLE_IMAGE="${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:2.10.3}"
ZONE="cm-e2e-$(openssl rand -hex 3).${NODERINGS_TEST_DOMAIN:-example.com}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/deploy/cert-manager-webhook-noderings"
WORK="$(mktemp -d)"
export KUBECONFIG="$WORK/kubeconfig"
ZONE_ID=""

log() { printf '\n== %s\n' "$*"; }

api() { # method path [json]
  local args=(-fsS -X "$1" -H "Authorization: Bearer $NODERINGS_TOKEN" -H "Content-Type: application/json")
  [[ -n "${NODERINGS_ORGANIZATION_ID:-}" ]] && args+=(-H "X-Organization-ID: $NODERINGS_ORGANIZATION_ID")
  [[ $# -ge 3 ]] && args+=(-d "$3")
  curl "${args[@]}" "$API$2"
}

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]] && kubectl version >/dev/null 2>&1; then
    log "Diagnostics"
    kubectl get certificate,certificaterequest,order,challenge -A 2>&1 | head -20 || true
    kubectl describe challenge -A 2>&1 | grep -E 'Reason|State|Presented|Message' | head -20 || true
    kubectl -n cert-manager logs deploy/noderings-webhook-cert-manager-webhook-noderings --tail=30 2>&1 || true
  fi
  log "Cleanup"
  if [[ -n "$ZONE_ID" ]]; then
    api DELETE "/v1/dns/zones/$ZONE_ID?force=true" >/dev/null && echo "deleted zone $ZONE"
  fi
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 && echo "deleted kind cluster $CLUSTER"
  fi
  rm -rf -- "$WORK"
  [[ $rc -eq 0 ]] && printf '\nEND TO END PASSED\n' || printf '\nEND TO END FAILED (exit %d)\n' "$rc"
}
trap cleanup EXIT

NS_IP="$(dig +short "$NAMESERVER" A | head -1)"
[[ -n "$NS_IP" ]] || { echo "cannot resolve $NAMESERVER" >&2; exit 1; }

log "Zone $ZONE"
ZONE_ID="$(api POST /v1/dns/zones "{\"name\":\"$ZONE\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["zone"]["id"])')"
echo "   id $ZONE_ID"

log "kind cluster $CLUSTER"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind export kubeconfig --name "$CLUSTER" >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "cert-manager $CERT_MANAGER_VERSION"
# Look up challenge records on the NodeRings nameserver itself: the test zone is not delegated.
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo update jetstack >/dev/null
helm upgrade --install cert-manager jetstack/cert-manager --version "$CERT_MANAGER_VERSION" \
  -n cert-manager --create-namespace --set crds.enabled=true \
  --set 'extraArgs={--dns01-recursive-nameservers-only,--dns01-recursive-nameservers='"$NS_IP"':53}' \
  --wait --timeout 5m

log "Pebble ACME server, validating against $NAMESERVER ($NS_IP)"
kubectl create namespace pebble --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: pebble, namespace: pebble}
spec:
  selector: {matchLabels: {app: pebble}}
  template:
    metadata: {labels: {app: pebble}}
    spec:
      containers:
        - name: pebble
          image: $PEBBLE_IMAGE
          args: ["-dnsserver", "$NS_IP:53"]
          env:
            - {name: PEBBLE_VA_NOSLEEP, value: "1"}
            - {name: PEBBLE_WFE_NONCEREJECT, value: "0"}
          ports: [{containerPort: 14000}]
---
apiVersion: v1
kind: Service
metadata: {name: pebble, namespace: pebble}
spec:
  selector: {app: pebble}
  ports: [{port: 14000, targetPort: 14000}]
EOF
kubectl -n pebble rollout status deploy/pebble --timeout 3m

log "NodeRings webhook (chart from this repository)"
kubectl -n cert-manager create secret generic noderings-token --from-literal=token="$NODERINGS_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install noderings-webhook "$CHART" -n cert-manager \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  --wait --timeout 3m
for _ in $(seq 1 30); do
  kubectl get apiservice v1alpha1.acme.noderings.com -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q True && break
  sleep 5
done
kubectl get apiservice v1alpha1.acme.noderings.com

log "ClusterIssuer and Certificate for $ZONE and *.$ZONE"
kubectl apply -f - >/dev/null <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: noderings-pebble}
spec:
  acme:
    server: https://pebble.pebble.svc.cluster.local:14000/dir
    skipTLSVerify: true # Pebble's own test certificate
    privateKeySecretRef: {name: noderings-pebble-account}
    solvers:
      - dns01:
          webhook:
            groupName: acme.noderings.com
            solverName: noderings
            config:
              tokenSecretRef: {name: noderings-token, key: token}
              ttl: 60
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: e2e, namespace: default}
spec:
  secretName: e2e-tls
  issuerRef: {name: noderings-pebble, kind: ClusterIssuer}
  dnsNames: ["$ZONE", "*.$ZONE"]
EOF
kubectl wait --for=condition=Ready certificate/e2e -n default --timeout 6m
kubectl get certificate e2e -n default

log "Issued certificate"
kubectl get secret e2e-tls -n default -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/tls.crt"
openssl x509 -in "$WORK/tls.crt" -noout -issuer -ext subjectAltName | sed 's/^/   /'
sans="$(openssl x509 -in "$WORK/tls.crt" -noout -ext subjectAltName)"
[[ "$sans" == *"DNS:$ZONE"* && "$sans" == *"DNS:*.$ZONE"* ]] || { echo "   missing SANs" >&2; exit 1; }

log "Challenge records are cleaned up"
for _ in $(seq 1 24); do
  left="$(dig +short @"$NAMESERVER" "_acme-challenge.$ZONE" TXT)"
  [[ -z "$left" ]] && break
  sleep 5
done
[[ -z "$left" ]] || { echo "   _acme-challenge.$ZONE still has: $left" >&2; exit 1; }
echo "   _acme-challenge.$ZONE TXT -> <none>"

log "Webhook log"
kubectl -n cert-manager logs deploy/noderings-webhook-cert-manager-webhook-noderings --tail=200 \
  | grep -E 'presented|removed|error' | cut -c1-200 | head -12
