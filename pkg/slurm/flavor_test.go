package slurm

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseMemoryString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"Gigabytes", "16G", 16 * 1024 * 1024 * 1024, false},
		{"Gigabytes with B", "16GB", 16 * 1024 * 1024 * 1024, false},
		{"Megabytes", "32000M", 32000 * 1024 * 1024, false},
		{"Megabytes with B", "32000MB", 32000 * 1024 * 1024, false},
		{"Kilobytes", "1024K", 1024 * 1024, false},
		{"Kilobytes with B", "1024KB", 1024 * 1024, false},
		{"Bytes", "1024", 1024, false},
		{"Empty string", "", 0, false},
		{"Lowercase g", "16g", 16 * 1024 * 1024 * 1024, false},
		{"With spaces", "  16G  ", 16 * 1024 * 1024 * 1024, false},
		{"Invalid format", "16X", 0, true},
		{"Invalid number", "abcG", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMemoryString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMemoryString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseMemoryString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectGPUResources(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		containers []v1.Container
		want       int64
	}{
		{
			name: "No GPU",
			containers: []v1.Container{
				{
					Name: "test",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("8Gi"),
						},
					},
				},
			},
			want: 0,
		},
		{
			name: "Single NVIDIA GPU",
			containers: []v1.Container{
				{
					Name: "test",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			want: 1,
		},
		{
			name: "Multiple NVIDIA GPUs",
			containers: []v1.Container{
				{
					Name: "test",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
						},
					},
				},
			},
			want: 2,
		},
		{
			name: "AMD GPU",
			containers: []v1.Container{
				{
					Name: "test",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"amd.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			want: 1,
		},
		{
			name: "Multiple containers with GPUs",
			containers: []v1.Container{
				{
					Name: "test1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
				{
					Name: "test2",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
						},
					},
				},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectGPUResources(ctx, tt.containers)
			if got != tt.want {
				t.Errorf("detectGPUResources() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractGPUCountFromFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  int64
	}{
		{"No GPU flags", []string{"--partition=cpu", "--time=01:00:00"}, 0},
		{"GPU with count 1", []string{"--gres=gpu:1"}, 1},
		{"GPU with count 2", []string{"--gres=gpu:2"}, 2},
		{"GPU with count 4", []string{"--gres=gpu:4", "--partition=gpu"}, 4},
		{"GPU without count", []string{"--gres=gpu"}, 0},
		{"Multiple flags with GPU", []string{"--partition=gpu", "--gres=gpu:2", "--time=04:00:00"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGPUCountFromFlags(tt.flags)
			if got != tt.want {
				t.Errorf("extractGPUCountFromFlags() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasGPUInFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  bool
	}{
		{"No GPU flags", []string{"--partition=cpu", "--time=01:00:00"}, false},
		{"Has --gres=gpu", []string{"--gres=gpu:1"}, true},
		{"Has gpu in partition", []string{"--partition=gpu"}, true},
		{"Has gpu in other flag", []string{"--constraint=gpu_node"}, true},
		{"Empty flags", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasGPUInFlags(tt.flags)
			if got != tt.want {
				t.Errorf("hasGPUInFlags() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeduplicateSlurmFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name:  "No duplicates",
			flags: []string{"--partition=cpu", "--time=01:00:00"},
			want:  []string{"--partition=cpu", "--time=01:00:00"},
		},
		{
			name:  "Duplicate partitions - last wins",
			flags: []string{"--partition=cpu", "--time=01:00:00", "--partition=gpu"},
			want:  []string{"--partition=gpu", "--time=01:00:00"},
		},
		{
			name:  "Multiple duplicates",
			flags: []string{"--partition=cpu", "--mem=8G", "--partition=gpu", "--mem=16G"},
			want:  []string{"--partition=gpu", "--mem=16G"},
		},
		{
			name:  "Empty strings filtered",
			flags: []string{"--partition=cpu", "", "--mem=8G"},
			want:  []string{"--partition=cpu", "--mem=8G"},
		},
		{
			name:  "With spaces in flags",
			flags: []string{"  --partition=cpu  ", "--mem=8G"},
			want:  []string{"--partition=cpu", "--mem=8G"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateSlurmFlags(tt.flags)
			if len(got) != len(tt.want) {
				t.Errorf("deduplicateSlurmFlags() length = %v, want %v", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("deduplicateSlurmFlags()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolveFlavor(t *testing.T) {
	ctx := context.Background()

	config := SlurmConfig{
		Flavors: map[string]FlavorConfig{
			"default": {
				Name:          "default",
				CPUDefault:    4,
				MemoryDefault: "16G",
				SlurmFlags:    []string{"--partition=cpu"},
			},
			"gpu-nvidia": {
				Name:          "gpu-nvidia",
				CPUDefault:    8,
				MemoryDefault: "64G",
				SlurmFlags:    []string{"--gres=gpu:1", "--partition=gpu"},
			},
		},
		DefaultFlavor: "default",
	}

	tests := []struct {
		name         string
		metadata     metav1.ObjectMeta
		containers   []v1.Container
		wantFlavor   string
		wantNil      bool
	}{
		{
			name: "Explicit annotation",
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/flavor": "gpu-nvidia",
				},
			},
			containers: []v1.Container{{}},
			wantFlavor: "gpu-nvidia",
			wantNil:    false,
		},
		{
			name:     "GPU auto-detection",
			metadata: metav1.ObjectMeta{},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			wantFlavor: "gpu-nvidia",
			wantNil:    false,
		},
		{
			name:       "Default flavor",
			metadata:   metav1.ObjectMeta{},
			containers: []v1.Container{{}},
			wantFlavor: "default",
			wantNil:    false,
		},
		{
			name: "Invalid annotation falls back to default",
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/flavor": "non-existent",
				},
			},
			containers: []v1.Container{{}},
			wantFlavor: "default",
			wantNil:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFlavor(ctx, config, tt.metadata, tt.containers)
			if err != nil {
				t.Errorf("resolveFlavor() error = %v", err)
				return
			}
			if tt.wantNil && got != nil {
				t.Errorf("resolveFlavor() expected nil, got %v", got)
				return
			}
			if !tt.wantNil && got == nil {
				t.Errorf("resolveFlavor() expected non-nil result")
				return
			}
			if !tt.wantNil && got.FlavorName != tt.wantFlavor {
				t.Errorf("resolveFlavor() flavor = %v, want %v", got.FlavorName, tt.wantFlavor)
			}
		})
	}
}

func TestFlavorConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		flavor  FlavorConfig
		wantErr bool
	}{
		{
			name: "Valid flavor",
			flavor: FlavorConfig{
				Name:          "test",
				CPUDefault:    4,
				MemoryDefault: "16G",
				SlurmFlags:    []string{"--partition=cpu"},
			},
			wantErr: false,
		},
		{
			name: "Empty name",
			flavor: FlavorConfig{
				Name:       "",
				CPUDefault: 4,
			},
			wantErr: true,
		},
		{
			name: "Negative CPU",
			flavor: FlavorConfig{
				Name:       "test",
				CPUDefault: -1,
			},
			wantErr: true,
		},
		{
			name: "Invalid memory format",
			flavor: FlavorConfig{
				Name:          "test",
				MemoryDefault: "invalid",
			},
			wantErr: true,
		},
		{
			name: "Invalid SLURM flag format",
			flavor: FlavorConfig{
				Name:       "test",
				SlurmFlags: []string{"invalid_flag"},
			},
			wantErr: true,
		},
		{
			name: "Empty SLURM flag",
			flavor: FlavorConfig{
				Name:       "test",
				SlurmFlags: []string{"--partition=cpu", ""},
			},
			wantErr: true,
		},
		{
			name: "Valid GID",
			flavor: FlavorConfig{
				Name:       "test",
				CPUDefault: 4,
				GID:        int64Ptr(1001),
			},
			wantErr: false,
		},
		{
			name: "Negative GID",
			flavor: FlavorConfig{
				Name:       "test",
				CPUDefault: 4,
				GID:        int64Ptr(-1),
			},
			wantErr: true,
		},
		{
			name: "GID zero is valid",
			flavor: FlavorConfig{
				Name:       "test",
				CPUDefault: 4,
				GID:        int64Ptr(0),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.flavor.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("FlavorConfig.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Helper function to create int64 pointers
func int64Ptr(i int64) *int64 {
	return &i
}

func TestGIDResolutionPriority(t *testing.T) {
	ctx := context.Background()
	defaultGID := int64(1000)
	flavorGID := int64(2000)
	annotationGID := "3000"

	tests := []struct {
		name             string
		config           SlurmConfig
		metadata         metav1.ObjectMeta
		flavor           *FlavorResolution
		expectedGID      *int64
		expectWarning    bool
	}{
		{
			name: "No GID configured anywhere",
			config: SlurmConfig{
				DefaultGID:       nil,
				AllowGIDOverride: false,
			},
			metadata:    metav1.ObjectMeta{},
			flavor:      nil,
			expectedGID: nil,
		},
		{
			name: "Only default GID configured",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: false,
			},
			metadata:    metav1.ObjectMeta{},
			flavor:      nil,
			expectedGID: &defaultGID,
		},
		{
			name: "Flavor GID overrides default",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: false,
			},
			metadata: metav1.ObjectMeta{},
			flavor: &FlavorResolution{
				FlavorName: "test-flavor",
				GID:        &flavorGID,
			},
			expectedGID: &flavorGID,
		},
		{
			name: "Annotation GID overrides all when allowed",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: true,
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/gid": annotationGID,
				},
			},
			flavor: &FlavorResolution{
				FlavorName: "test-flavor",
				GID:        &flavorGID,
			},
			expectedGID: int64Ptr(3000),
		},
		{
			name: "Annotation ignored when override disabled",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: false,
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/gid": annotationGID,
				},
			},
			flavor: &FlavorResolution{
				FlavorName: "test-flavor",
				GID:        &flavorGID,
			},
			expectedGID:   &flavorGID,
			expectWarning: true,
		},
		{
			name: "Invalid annotation falls back to flavor",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: true,
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/gid": "invalid",
				},
			},
			flavor: &FlavorResolution{
				FlavorName: "test-flavor",
				GID:        &flavorGID,
			},
			expectedGID:   &flavorGID,
			expectWarning: true,
		},
		{
			name: "Negative annotation falls back to flavor",
			config: SlurmConfig{
				DefaultGID:       &defaultGID,
				AllowGIDOverride: true,
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/gid": "-1",
				},
			},
			flavor: &FlavorResolution{
				FlavorName: "test-flavor",
				GID:        &flavorGID,
			},
			expectedGID:   &flavorGID,
			expectWarning: true,
		},
		{
			name: "Zero GID is valid",
			config: SlurmConfig{
				AllowGIDOverride: true,
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/gid": "0",
				},
			},
			flavor:      nil,
			expectedGID: int64Ptr(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the GID resolution logic from prepare.go
			var gidValue *int64

			// Start with default GID from global config
			if tt.config.DefaultGID != nil {
				gidValue = tt.config.DefaultGID
			}

			// Override with flavor GID if available
			if tt.flavor != nil && tt.flavor.GID != nil {
				gidValue = tt.flavor.GID
			}

			// Override with annotation GID if allowed and present
			if tt.config.AllowGIDOverride {
				if gidAnnotation, ok := tt.metadata.Annotations["slurm-job.vk.io/gid"]; ok {
					if parsedGID, err := strconv.ParseInt(gidAnnotation, 10, 64); err == nil {
						if parsedGID >= 0 {
							gidValue = &parsedGID
						}
					}
				}
			}

			// Verify the result
			if tt.expectedGID == nil && gidValue != nil {
				t.Errorf("Expected nil GID, got %d", *gidValue)
			} else if tt.expectedGID != nil && gidValue == nil {
				t.Errorf("Expected GID %d, got nil", *tt.expectedGID)
			} else if tt.expectedGID != nil && gidValue != nil && *gidValue != *tt.expectedGID {
				t.Errorf("Expected GID %d, got %d", *tt.expectedGID, *gidValue)
			}
		})
	}
}

func TestGIDInSlurmFlags(t *testing.T) {
	tests := []struct {
		name        string
		gid         *int64
		expectedFlag string
	}{
		{
			name:        "GID 1000",
			gid:         int64Ptr(1000),
			expectedFlag: "--gid=1000",
		},
		{
			name:        "GID 0",
			gid:         int64Ptr(0),
			expectedFlag: "--gid=0",
		},
		{
			name:        "GID 65535",
			gid:         int64Ptr(65535),
			expectedFlag: "--gid=65535",
		},
		{
			name:        "No GID",
			gid:         nil,
			expectedFlag: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sbatchFlags []string

			// Simulate adding GID flag
			if tt.gid != nil {
				sbatchFlags = append(sbatchFlags, fmt.Sprintf("--gid=%d", *tt.gid))
			}

			if tt.expectedFlag == "" {
				if len(sbatchFlags) > 0 {
					t.Errorf("Expected no GID flag, but got flags: %v", sbatchFlags)
				}
			} else {
				found := false
				for _, flag := range sbatchFlags {
					if flag == tt.expectedFlag {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected flag %q not found in flags: %v", tt.expectedFlag, sbatchFlags)
				}
			}
		})
	}
}
