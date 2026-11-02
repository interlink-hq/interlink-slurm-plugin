package batchsystem

import (
	"context"
	"fmt"
)

// BatchSystemType identifies the type of batch scheduling system to use.
type BatchSystemType string

const (
	// BatchSystemSLURM represents the SLURM workload manager
	BatchSystemSLURM BatchSystemType = "slurm"

	// BatchSystemPBS represents PBS/Torque batch systems
	BatchSystemPBS BatchSystemType = "pbs"

	// BatchSystemLSF represents IBM Spectrum LSF
	BatchSystemLSF BatchSystemType = "lsf"

	// BatchSystemCondor represents HTCondor
	BatchSystemCondor BatchSystemType = "condor"
)

// FactoryConfig contains the configuration needed to instantiate a batch system.
// This includes the type of batch system and a generic configuration map that
// will be type-asserted to the specific configuration struct for each implementation.
type FactoryConfig struct {
	Type   BatchSystemType // The type of batch system to create
	Config interface{}     // Batch system-specific configuration (will be type-asserted)
}

// Factory is responsible for creating instances of batch system implementations.
// It uses the factory pattern to abstract the instantiation of different batch systems.
type Factory struct {
	constructors map[BatchSystemType]Constructor
}

// Constructor is a function type that creates a new BatchSystem instance from a configuration.
type Constructor func(ctx context.Context, config interface{}) (BatchSystem, error)

// NewFactory creates a new Factory instance with no registered constructors.
// Batch system implementations must register themselves using RegisterBatchSystem.
func NewFactory() *Factory {
	return &Factory{
		constructors: make(map[BatchSystemType]Constructor),
	}
}

// RegisterBatchSystem registers a constructor function for a specific batch system type.
// This allows batch system implementations to register themselves at initialization time.
//
// Example usage:
//   factory := batchsystem.NewFactory()
//   factory.RegisterBatchSystem(batchsystem.BatchSystemSLURM, slurm.NewSlurmBatchSystem)
func (f *Factory) RegisterBatchSystem(bsType BatchSystemType, constructor Constructor) {
	f.constructors[bsType] = constructor
}

// Create instantiates a new BatchSystem based on the provided configuration.
// It looks up the registered constructor for the specified type and invokes it.
// Returns an error if the batch system type is not registered or if construction fails.
func (f *Factory) Create(ctx context.Context, config FactoryConfig) (BatchSystem, error) {
	constructor, exists := f.constructors[config.Type]
	if !exists {
		return nil, fmt.Errorf("unsupported batch system type: %s", config.Type)
	}

	batchSystem, err := constructor(ctx, config.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create batch system %s: %w", config.Type, err)
	}

	return batchSystem, nil
}

// GetRegisteredTypes returns a list of all registered batch system types.
// This is useful for validation and displaying available options to users.
func (f *Factory) GetRegisteredTypes() []BatchSystemType {
	types := make([]BatchSystemType, 0, len(f.constructors))
	for bsType := range f.constructors {
		types = append(types, bsType)
	}
	return types
}

// DefaultFactory is a global factory instance that can be used throughout the application.
// Batch system implementations can register themselves with this factory during init().
var DefaultFactory = NewFactory()

// RegisterDefault is a convenience function to register a batch system with the default factory.
func RegisterDefault(bsType BatchSystemType, constructor Constructor) {
	DefaultFactory.RegisterBatchSystem(bsType, constructor)
}

// CreateBatchSystem is a convenience function to create a batch system using the default factory.
func CreateBatchSystem(ctx context.Context, config FactoryConfig) (BatchSystem, error) {
	return DefaultFactory.Create(ctx, config)
}
