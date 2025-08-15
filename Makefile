all: sidecar

sidecar:
	CGO_ENABLED=0 GOOS=linux go build -o bin/slurm-sd cmd/main.go

test:
	dagger call -m ./ci  test --interlink-version 0.5.2-pre2 --src ./ --plugin-config ./ci/manifests/plugin-config.yaml --manifests ./ci/manifests
