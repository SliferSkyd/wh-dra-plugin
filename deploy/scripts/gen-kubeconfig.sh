#!/usr/bin/env bash
# Generate a kubeconfig for a new team member using a ServiceAccount token.
#
# Usage:
#   ./deploy/scripts/gen-kubeconfig.sh <username> [namespace]
#
# Example:
#   ./deploy/scripts/gen-kubeconfig.sh alice t3k-workloads
#
# Output: kubeconfig-<username>.yaml  (send this file to the user)

set -euo pipefail

USERNAME=${1:?Usage: $0 <username> [namespace]}
NAMESPACE=${2:-t3k-workloads}
OUTFILE="kubeconfig-${USERNAME}.yaml"

CLUSTER_SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CLUSTER_NAME=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')

echo "Creating ServiceAccount ${USERNAME} in ${NAMESPACE}..."
kubectl create serviceaccount "${USERNAME}" -n "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Binding roles..."
kubectl create rolebinding "${USERNAME}-developer" \
  --role=t3k-developer \
  --serviceaccount="${NAMESPACE}:${USERNAME}" \
  --namespace="${NAMESPACE}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create clusterrolebinding "${USERNAME}-device-reader" \
  --clusterrole=t3k-device-reader \
  --serviceaccount="${NAMESPACE}:${USERNAME}" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Creating token secret..."
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${USERNAME}-token
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${USERNAME}
type: kubernetes.io/service-account-token
EOF

echo "Waiting for token..."
for i in $(seq 1 10); do
  TOKEN=$(kubectl get secret "${USERNAME}-token" -n "${NAMESPACE}" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
  [ -n "${TOKEN}" ] && break
  sleep 1
done

CA_DATA=$(kubectl get secret "${USERNAME}-token" -n "${NAMESPACE}" \
  -o jsonpath='{.data.ca\.crt}')

cat > "${OUTFILE}" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: ${CLUSTER_NAME}
  cluster:
    server: ${CLUSTER_SERVER}
    certificate-authority-data: ${CA_DATA}
contexts:
- name: ${USERNAME}@${CLUSTER_NAME}
  context:
    cluster: ${CLUSTER_NAME}
    user: ${USERNAME}
    namespace: ${NAMESPACE}
current-context: ${USERNAME}@${CLUSTER_NAME}
users:
- name: ${USERNAME}
  user:
    token: ${TOKEN}
EOF

echo ""
echo "Done! Send ${OUTFILE} to ${USERNAME}."
echo "They run: cp ${OUTFILE} ~/.kube/config"
