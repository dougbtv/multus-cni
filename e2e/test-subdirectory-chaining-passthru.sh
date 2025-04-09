#!/bin/sh
set -o errexit
set -o pipefail
set -o nounset

export PATH=${PATH}:./bin

TEST_POD_NAME="sysctl-modified"

# Reconfigure multus
kubectl apply -f yamls/subdirectory-chain-passthru-configupdate.yml

# Restart the multus daemonset to pick up the new config
kubectl rollout restart daemonset kube-multus-ds -n kube-system
kubectl rollout status daemonset/kube-multus-ds -n kube-system

# Deploy the daemonset that will lay down the chained CNI config
kubectl apply -f yamls/subdirectory-chaining-passthru.yml

# Wait for the daemonset pods to be ready (make sure they set up CNI config)
kubectl rollout status daemonset/cni-setup-daemonset

# Deploy a test pod that will get chained CNI applied
kubectl apply -f yamls/subdirectory-chaining-pod.yml

# Wait for the pod to be Ready
kubectl wait --for=condition=ready pod/sysctl-modified --timeout=300s

# Check that the sysctl got set
echo "Verifying sysctl arp_filter is set to 1 on eth0"

SYSCTL_VALUE=$(kubectl exec sysctl-modified -- sysctl -n net.ipv4.conf.eth0.arp_filter)

if [ "$SYSCTL_VALUE" != "1" ]; then
  echo "FAIL: net.ipv4.conf.eth0.arp_filter is not set to 1, got ${SYSCTL_VALUE}" >&2
  exit 1
else
  echo "SUCCESS: net.ipv4.conf.eth0.arp_filter is set correctly."
fi

# Remove the rest...
echo "Cleaning up test resources"
kubectl delete -f yamls/subdirectory-chaining-pod.yml
kubectl delete -f yamls/subdirectory-chaining-passthru.yml

exit 0
