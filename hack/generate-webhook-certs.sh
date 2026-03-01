#!/bin/bash
# Script to generate self-signed certificates for the webhook
# Run this before deploying the webhook

set -e

NAMESPACE="recluster4-system"
SERVICE="recluster4-webhook-service"
SECRET="webhook-server-cert"

# Create namespace if it doesn't exist
kubectl create namespace ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -

# Generate certificates
TMPDIR=$(mktemp -d)
cd ${TMPDIR}

# Generate CA
openssl genrsa -out ca.key 2048
openssl req -x509 -new -nodes -key ca.key -sha256 -days 365 -out ca.crt -subj "/CN=webhook-ca"

# Generate server key and CSR
openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr -subj "/CN=${SERVICE}.${NAMESPACE}.svc" \
  -config <(cat <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${SERVICE}.${NAMESPACE}.svc
DNS.4 = ${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF
)

# Sign server cert with CA
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 365 -sha256 \
  -extfile <(cat <<EOF
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${SERVICE}.${NAMESPACE}.svc
DNS.4 = ${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF
)

# Create/update secret (generic to include ca.crt alongside tls.crt/tls.key)
kubectl create secret generic ${SECRET} \
  --from-file=tls.crt=server.crt \
  --from-file=tls.key=server.key \
  --from-file=ca.crt=ca.crt \
  --namespace=${NAMESPACE} \
  --dry-run=client -o yaml | kubectl apply -f -

# Get CA bundle for webhook config
CA_BUNDLE=$(cat ca.crt | base64 | tr -d '\n')
echo ""
echo "CA Bundle (use this in MutatingWebhookConfiguration):"
echo "${CA_BUNDLE}"

# Patch the MutatingWebhookConfiguration with the CA bundle
# Try the kustomize-generated name first, then the standalone name
if kubectl get mutatingwebhookconfiguration recluster4-recluster-pod-scheduling-gate &>/dev/null; then
  kubectl patch mutatingwebhookconfiguration recluster4-recluster-pod-scheduling-gate \
    --type='json' \
    -p="[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"${CA_BUNDLE}\"}]"
elif kubectl get mutatingwebhookconfiguration recluster-pod-scheduling-gate &>/dev/null; then
  kubectl patch mutatingwebhookconfiguration recluster-pod-scheduling-gate \
    --type='json' \
    -p="[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"${CA_BUNDLE}\"}]"
else
  echo "MutatingWebhookConfiguration not found yet - apply it first then run this script again"
fi

# Cleanup
cd -
rm -rf ${TMPDIR}

echo ""
echo "Done! Secret '${SECRET}' created in namespace '${NAMESPACE}'"
