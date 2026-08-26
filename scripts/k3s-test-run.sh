#!/bin/bash
# k3s-test-run.sh - Run integration tests on K3s cluster
#
# Usage: ./scripts/k3s-test-run.sh
# Requirements: k3s-test-setup.sh must have been run first

set -e

# Get test directory from previous setup
if [ -f /tmp/interlink-test-dir.txt ]; then
  TEST_DIR=$(cat /tmp/interlink-test-dir.txt)
else
  echo "ERROR: Test directory not found. Did you run k3s-test-setup.sh first?"
  exit 1
fi

echo "=== Running interLink SLURM plugin integration tests ==="
echo "Test directory: ${TEST_DIR}"

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# Check cluster status
echo "Checking cluster status..."
kubectl get nodes
kubectl get pods -A

# Wait for virtual-kubelet node to be present and Ready
echo "Waiting for virtual-kubelet node..."
for i in $(seq 1 30); do
  if kubectl get node virtual-kubelet &>/dev/null; then
    echo "Virtual-kubelet node found!"
    break
  fi
  echo "Waiting for virtual-kubelet node... ($i/30)"
  sleep 2
done

kubectl get node virtual-kubelet || {
  echo "ERROR: virtual-kubelet node not found!"
  echo "All nodes:"
  kubectl get nodes || true
  echo "All pods:"
  kubectl get pods -A || true
  echo "VK process (host):"
  VK_PID_FILE="${TEST_DIR}/vk.pid"
  if [ -f "${VK_PID_FILE}" ]; then
    VK_PID=$(cat "${VK_PID_FILE}")
    echo "VK PID ${VK_PID} running: $(kill -0 "${VK_PID}" 2>/dev/null && echo yes || echo no)"
    echo "VK logs (last 50 lines):"
    tail -50 "${TEST_DIR}/vk.log" || true
  fi
  echo "interLink API logs:"
  docker logs interlink-api --tail=50 || true
  exit 1
}

# Ensure the node is Ready before running tests
echo "Waiting for virtual-kubelet node to be Ready..."
if ! kubectl wait --for=condition=Ready node/virtual-kubelet --timeout=120s; then
  echo "ERROR: virtual-kubelet node is not Ready"
  echo "Node status:"
  kubectl describe node virtual-kubelet || true
  VK_PID_FILE="${TEST_DIR}/vk.pid"
  if [ -f "${VK_PID_FILE}" ]; then
    echo "VK logs (last 100 lines):"
    tail -100 "${TEST_DIR}/vk.log" || true
  fi
  echo "interLink API logs:"
  docker logs interlink-api --tail=50 || true
  echo "SLURM plugin logs:"
  docker logs interlink-plugin --tail=50 || true
  exit 1
fi
echo "✓ virtual-kubelet node is Ready"

# Approve any pending CSRs
echo "Approving CSRs..."
kubectl get csr -o name | xargs -r kubectl certificate approve || true

# Get project root to access test configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Clone vk-test-set into the test directory
VK_TEST_DIR="${TEST_DIR}/vk-test-set"
if [ ! -d "${VK_TEST_DIR}" ]; then
  echo "Cloning vk-test-set test suite..."
  git clone https://github.com/interlink-hq/vk-test-set.git "${VK_TEST_DIR}"
fi

cd "${VK_TEST_DIR}"

# Create test configuration with SLURM-specific settings
echo "Creating test configuration..."
cat > vktest_config.yaml <<EOF
target_nodes:
  - virtual-kubelet

required_namespaces:
  - default
  - kube-system

timeout_multiplier: 10.
values:
  namespace: default

  annotations:
    slurm-job.vk.io/flags: "--job-name=test-pod-cfg -t 2800"
    slurm-job.vk.io/image-root: "docker://"

  tolerations:
    - key: virtual-node.interlink/no-schedule
      operator: Exists
      effect: NoSchedule
EOF

# Setup Python virtual environment and install vk-test-set
echo "Setting up Python environment..."
python3 -m venv .venv
source .venv/bin/activate
pip3 install -e ./ || {
  echo "ERROR: Failed to install vk-test-set"
  exit 1
}

echo "vk-test-set installed successfully"

# Run tests
echo "Running integration tests..."
echo "========================================="

# Run pytest (excluding tests not applicable to SLURM plugin)
source .venv/bin/activate
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
pytest -v -k "not rclone and not limits and not stress and not multi-init and not fail" 2>&1 | tee "${TEST_DIR}/test-results.log"
TEST_EXIT_CODE=${PIPESTATUS[0]}

echo "========================================="
echo ""

if [ ${TEST_EXIT_CODE} -eq 0 ]; then
  echo "✓ All tests passed!"
else
  echo "✗ Some tests failed (exit code: ${TEST_EXIT_CODE})"
  echo ""
  echo "Check logs for details:"
  echo "  - Test results:  ${TEST_DIR}/test-results.log"
  echo "  - Plugin logs:   ${TEST_DIR}/interlink-plugin.log"
  echo "  - API logs:      ${TEST_DIR}/interlink-api.log"
  echo "  - VK logs:       ${TEST_DIR}/vk.log"
fi

exit ${TEST_EXIT_CODE}
