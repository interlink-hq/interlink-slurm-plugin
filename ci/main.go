// A generated module for InterlinkSlurm functions
//
// This module has been generated via dagger init and serves as a reference to
// basic module structure as you get started with Dagger.
//
// Two functions have been pre-created. You can modify, delete, or add to them,
// as needed. They demonstrate usage of arguments and return types using simple
// echo and grep commands. The functions can be called from the dagger CLI or
// from one of the SDKs.
//
// The first line in this comment block is a short description line and the
// rest is a long description with more detail on the module's purpose or usage,
// if appropriate. All modules should have a short description.

package main

import (
	"context"
	"dagger/interlink-slurm/internal/dagger"
	"time"
)

type InterlinkSlurm struct{}

// UnitTest runs the unit tests using make test
func (m *InterlinkSlurm) UnitTest(ctx context.Context, src *dagger.Directory) (string, error) {
	return dag.Container().
		From("golang:1.24").
		WithDirectory("/src", src).
		WithWorkdir("/src").
		WithExec([]string{"make", "test"}).
		Stdout(ctx)
}

func (m *InterlinkSlurm) RunTest(ctx context.Context, interlinkVersion string, pluginService *dagger.Service, manifests *dagger.Directory, interlinkEndpoint *dagger.Service, pluginConfig *dagger.File, src *dagger.Directory,
) *dagger.Container {
	m.UnitTest(ctx, src)

	registry := dag.Container().From("registry").
		WithExposedPort(5000).AsService()

	return dag.Interlink("ci", dagger.InterlinkOpts{
		VirtualKubeletRef: "ghcr.io/interlink-hq/interlink/virtual-kubelet-inttw:" + interlinkVersion,
		InterlinkRef:      "ghcr.io/interlink-hq/interlink/interlink:" + interlinkVersion,
	}).NewInterlink(dagger.InterlinkNewInterlinkOpts{
		PluginEndpoint:    pluginService,
		Manifests:         manifests,
		InterlinkEndpoint: interlinkEndpoint,
		PluginConfig:      pluginConfig,
		LocalRegistry:     registry,
	}).Test(dagger.InterlinkTestOpts{
		Manifests:    manifests,
		SourceFolder: src,
	})
}

// Returns lines that match a pattern in the files of the provided Directory
func (m *InterlinkSlurm) Test(ctx context.Context, interlinkVersion string, src *dagger.Directory, pluginConfig *dagger.File, manifests *dagger.Directory,
	// +optional
	pluginEndpoint *dagger.Service,
	// +optional
	interlinkEndpoint *dagger.Service,
	// +optional
	// +defaultPath="./manifests/interlink-config.yaml"
	interlinkConfig *dagger.File,
) (string, error) {
	if pluginEndpoint == nil {

		// build using Dockerfile and publish to registry
		plugin := src.DockerBuild(dagger.DirectoryDockerBuildOpts{
			Dockerfile: "docker/Dockerfile",
		}).
			WithFile("/etc/interlink/InterLinkConfig.yaml", pluginConfig).
			WithEnvVariable("SLURMCONFIGPATH", "/etc/interlink/InterLinkConfig.yaml").
			WithEnvVariable("SHARED_FS", "true").
			WithExposedPort(4000)

		pluginEndpoint, err := plugin.AsService(dagger.ContainerAsServiceOpts{Args: []string{}, UseEntrypoint: true, InsecureRootCapabilities: true}).Start(ctx)
		if err != nil {
			return "", err
		}

		interlink := dag.Container().From("ghcr.io/interlink-hq/interlink/interlink:"+interlinkVersion).
			WithFile("/etc/interlink/InterLinkConfig.yaml", interlinkConfig).
			WithEnvVariable("BUST", time.Now().String()).
			WithServiceBinding("plugin", pluginEndpoint).
			WithEnvVariable("INTERLINKCONFIGPATH", "/etc/interlink/InterLinkConfig.yaml").
			WithExposedPort(3000)

		interlinkEndpoint, err = interlink.
			AsService(
				dagger.ContainerAsServiceOpts{
					UseEntrypoint:            true,
					InsecureRootCapabilities: true,
				}).Start(ctx)
		if err != nil {
			return "", err
		}
		return m.RunTest(ctx, interlinkVersion, pluginEndpoint, manifests, interlinkEndpoint, pluginConfig, src).Stdout(ctx)
	}

	return m.RunTest(ctx, interlinkVersion, pluginEndpoint, manifests, interlinkEndpoint, pluginConfig, src).Stdout(ctx)
}
