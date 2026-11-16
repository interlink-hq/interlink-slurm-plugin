package batchsystem

import (
	"context"
	"errors"
	"strings"
	"testing"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
)

type stubBatchSystem struct{ cfg interface{} }

func (s *stubBatchSystem) Initialize(context.Context) error { return nil }
func (s *stubBatchSystem) Create(context.Context, commonIL.RetrievedPodData) (CreateResult, error) {
	return CreateResult{}, nil
}
func (s *stubBatchSystem) Delete(context.Context, *v1.Pod) error { return nil }
func (s *stubBatchSystem) Status(context.Context, []*v1.Pod) ([]commonIL.PodStatus, error) {
	return nil, nil
}
func (s *stubBatchSystem) GetLogs(context.Context, *v1.Pod, string, LogOptions) ([]byte, error) {
	return nil, nil
}
func (s *stubBatchSystem) GetSystemInfo(context.Context) (string, error) { return "", nil }

func TestFactoryCreateUsesRegisteredConstructor(t *testing.T) {
	f := NewFactory()
	const stubType BatchSystemType = "stub"
	f.RegisterBatchSystem(stubType, func(_ context.Context, cfg interface{}) (BatchSystem, error) {
		return &stubBatchSystem{cfg: cfg}, nil
	})

	bs, err := f.Create(context.Background(), FactoryConfig{Type: stubType, Config: "my-config"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	stub, ok := bs.(*stubBatchSystem)
	if !ok {
		t.Fatalf("Create() returned %T, want *stubBatchSystem", bs)
	}
	if stub.cfg != "my-config" {
		t.Errorf("constructor received config %v, want %q", stub.cfg, "my-config")
	}
}

func TestFactoryCreateUnknownType(t *testing.T) {
	f := NewFactory()
	f.RegisterBatchSystem("known", func(context.Context, interface{}) (BatchSystem, error) {
		return &stubBatchSystem{}, nil
	})

	_, err := f.Create(context.Background(), FactoryConfig{Type: "unknown"})
	if err == nil {
		t.Fatal("Create() with unregistered type succeeded, want error")
	}
	if !strings.Contains(err.Error(), `"unknown"`) || !strings.Contains(err.Error(), "known") {
		t.Errorf("error %q should name the requested type and list registered ones", err)
	}
}

func TestFactoryCreateWrapsConstructorError(t *testing.T) {
	f := NewFactory()
	sentinel := errors.New("boom")
	f.RegisterBatchSystem("failing", func(context.Context, interface{}) (BatchSystem, error) {
		return nil, sentinel
	})

	_, err := f.Create(context.Background(), FactoryConfig{Type: "failing"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Create() error = %v, want wrapped %v", err, sentinel)
	}
}

func TestFactoryGetRegisteredTypesSorted(t *testing.T) {
	f := NewFactory()
	noop := func(context.Context, interface{}) (BatchSystem, error) { return &stubBatchSystem{}, nil }
	for _, name := range []BatchSystemType{"zeta", "alpha", "mid"} {
		f.RegisterBatchSystem(name, noop)
	}

	got := f.GetRegisteredTypes()
	want := []BatchSystemType{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("GetRegisteredTypes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetRegisteredTypes() = %v, want %v", got, want)
		}
	}
}

func TestRegisterDefault(t *testing.T) {
	const stubType BatchSystemType = "stub-default-test"
	RegisterDefault(stubType, func(context.Context, interface{}) (BatchSystem, error) {
		return &stubBatchSystem{}, nil
	})
	t.Cleanup(func() {
		DefaultFactory.mu.Lock()
		delete(DefaultFactory.constructors, stubType)
		DefaultFactory.mu.Unlock()
	})

	if _, err := CreateBatchSystem(context.Background(), FactoryConfig{Type: stubType}); err != nil {
		t.Fatalf("CreateBatchSystem() error = %v", err)
	}
}
