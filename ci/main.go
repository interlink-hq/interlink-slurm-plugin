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
)

type InterlinkSlurm struct{}

func (m *InterlinkSlurm) RunTest(interlinkVersion string, pluginService *dagger.Service, manifests *dagger.Directory, interlinkConfig *dagger.File, pluginConfig *dagger.File, src *dagger.Directory,
) *dagger.Container {
	registry := dag.Container().From("registry").
		WithExposedPort(5000).AsService()

	return dag.Interlink("ci", dagger.InterlinkOpts{
		VirtualKubeletRef: "ghcr.io/intertwin-eu/interlink/virtual-kubelet-inttw:" + interlinkVersion,
		InterlinkRef:      "ghcr.io/intertwin-eu/interlink/interlink:" + interlinkVersion,
	}).NewInterlink(dagger.InterlinkNewInterlinkOpts{
		PluginEndpoint:  pluginService,
		Manifests:       manifests,
		InterlinkConfig: interlinkConfig,
		PluginConfig:    pluginConfig,
		LocalRegistry:   registry,
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
	// +defaultPath="./manifests/plugin-config.yaml"
	interlinkConfig *dagger.File,
) (string, error) {
	if pluginEndpoint == nil {

		// build using Dockerfile and publish to registry
		plugin := dag.Container().
			Build(src, dagger.ContainerBuildOpts{
				Dockerfile: "docker/Dockerfile",
			}).
			WithFile("/etc/interlink/InterLinkConfig.yaml", pluginConfig).
			WithEnvVariable("SLURMCONFIGPATH", "/etc/interlink/InterLinkConfig.yaml").
			WithEnvVariable("SHARED_FS", "true").
			WithExposedPort(4000).
			WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

		pluginEndpoint, err := plugin.AsService().Start(ctx)
		if err != nil {
			return "", err
		}
		return m.RunTest(interlinkVersion, pluginEndpoint, manifests, interlinkConfig, pluginConfig, src).Stdout(ctx)
	}

	return m.RunTest(interlinkVersion, pluginEndpoint, manifests, interlinkConfig, pluginConfig, src).Stdout(ctx)
}
