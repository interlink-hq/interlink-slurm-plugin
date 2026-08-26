all: sidecar

sidecar:
	CGO_ENABLED=0 GOOS=linux go build -o bin/slurm-sd cmd/main.go

unit-test:
	go test -v ./...

integration-test:
	dagger call -m ./ci  test --interlink-version 0.5.2-pre2 --src ./ --plugin-config ./ci/manifests/plugin-config.yaml --manifests ./ci/manifests

test: unit-test

# K3s-based integration tests (individual steps)
test-k3s-setup:
	@echo "Setting up K3s test environment..."
	@bash ./scripts/k3s-test-setup.sh

test-k3s-run:
	@echo "Running K3s integration tests..."
	@bash ./scripts/k3s-test-run.sh

test-k3s-cleanup:
	@echo "Cleaning up K3s test environment..."
	@bash ./scripts/k3s-test-cleanup.sh

# Complete K3s integration test cycle
test-k3s:
	@status=0; \
	$(MAKE) test-k3s-setup || status=$$?; \
	if [ $$status -eq 0 ]; then \
		$(MAKE) test-k3s-run || status=$$?; \
	fi; \
	$(MAKE) test-k3s-cleanup || cleanup_status=$$?; \
	if [ $$status -eq 0 ] && [ -n "$$cleanup_status" ]; then \
		status=$$cleanup_status; \
	fi; \
	exit $$status
