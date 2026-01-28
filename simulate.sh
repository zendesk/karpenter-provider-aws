#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="${1:-example}"
SNAPSHOT="${2:-./sandbox.yml}"
INSTANCES=us-west-2-instances.yaml

# Use a local kubeconfig in CWD to avoid touching the user's main kubeconfig
KUBECONFIG_FILE="$(pwd)/kwok-$CLUSTER_NAME.kubeconfig"
export KUBECONFIG="$KUBECONFIG_FILE"

echo "=== Karpenter KWOK Simulator ==="
echo ""

# 1. Create KWOK cluster (binary runtime is fastest)
echo "Creating KWOK cluster '$CLUSTER_NAME'..."
echo "Using kubeconfig: $KUBECONFIG_FILE"
kwokctl create cluster --name="$CLUSTER_NAME" --runtime=binary 2>&1

# 3. Install Karpenter CRDs
echo "Installing Karpenter CRDs..."
kubectl apply -f pkg/apis/crds/

# 4. Build simulator if needed
if [[ ! -f ./simulator ]] || [[ ./cmd/simulator/main.go -nt ./simulator ]]; then
    echo "Building simulator..."
    go build -o simulator ./cmd/simulator/...
fi

echo ""
echo "=== Setup complete ==="
echo ""
echo "To run the simulator:"
echo "  KUBECONFIG=$KUBECONFIG_FILE ./simulator --from $SNAPSHOT --with-instances $INSTANCES --cluster $CLUSTER_NAME run"
echo ""
echo "In another terminal, interact with the cluster:"
echo "  export KUBECONFIG=$KUBECONFIG_FILE"
echo "  kubectl get nodes"
echo "  kubectl get pods -A"
echo ""
echo "To cleanup:"
echo "  kwokctl delete cluster --name=$CLUSTER_NAME"
echo ""

# 5. Run simulator (this will block until Ctrl+C)
echo "Starting simulator..."
./simulator --from "$SNAPSHOT" --with-instances "$INSTANCES" --cluster "$CLUSTER_NAME" run
