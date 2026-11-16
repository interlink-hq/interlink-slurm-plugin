package batchsystem

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// BatchSystemType identifies a batch scheduler backend.
type BatchSystemType string

const (
	// BatchSystemSLURM is the SLURM workload manager backend.
	BatchSystemSLURM BatchSystemType = "slurm"
)

// FactoryConfig selects which backend to build and carries its configuration.
// Config is passed untouched to the backend Constructor, which is responsible
// for asserting it to its own configuration type.
type FactoryConfig struct {
	Type   BatchSystemType
	Config interface{}
}

// Constructor builds a BatchSystem from a backend-specific configuration.
type Constructor func(ctx context.Context, config interface{}) (BatchSystem, error)

// Factory maps BatchSystemType values to their constructors.
type Factory struct {
	mu           sync.RWMutex
	constructors map[BatchSystemType]Constructor
}

// NewFactory returns an empty Factory.
func NewFactory() *Factory {
	return &Factory{constructors: make(map[BatchSystemType]Constructor)}
}

// RegisterBatchSystem registers (or replaces) the constructor for bsType.
func (f *Factory) RegisterBatchSystem(bsType BatchSystemType, constructor Constructor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.constructors[bsType] = constructor
}

// Create instantiates the backend selected by config.Type.
func (f *Factory) Create(ctx context.Context, config FactoryConfig) (BatchSystem, error) {
	f.mu.RLock()
	constructor, exists := f.constructors[config.Type]
	f.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("unsupported batch system type %q (registered: %v)", config.Type, f.GetRegisteredTypes())
	}

	bs, err := constructor(ctx, config.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create batch system %q: %w", config.Type, err)
	}
	return bs, nil
}

// GetRegisteredTypes returns the registered backend types in sorted order.
func (f *Factory) GetRegisteredTypes() []BatchSystemType {
	f.mu.RLock()
	defer f.mu.RUnlock()
	types := make([]BatchSystemType, 0, len(f.constructors))
	for bsType := range f.constructors {
		types = append(types, bsType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}

// DefaultFactory is the process-wide factory. Backends register themselves
// with it from their package init().
var DefaultFactory = NewFactory()

// RegisterDefault registers a constructor with DefaultFactory.
func RegisterDefault(bsType BatchSystemType, constructor Constructor) {
	DefaultFactory.RegisterBatchSystem(bsType, constructor)
}

// CreateBatchSystem builds a backend using DefaultFactory.
func CreateBatchSystem(ctx context.Context, config FactoryConfig) (BatchSystem, error) {
	return DefaultFactory.Create(ctx, config)
}
