package slurm

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStringToHex(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "test",
			expected: "74657374",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "string with spaces",
			input:    "a b",
			expected: "6162",
		},
		{
			name:     "special characters",
			input:    "a-b_c",
			expected: "612d625f63",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringToHex(tt.input)
			if result != tt.expected {
				t.Errorf("stringToHex(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParsingTimeFromString(t *testing.T) {
	ctx := context.Background()
	timestampFormat := "2006-01-02 15:04:05.999999999 -0700 MST"

	tests := []struct {
		name        string
		input       string
		shouldError bool
	}{
		{
			name:        "valid timestamp",
			input:       "2024-01-15 10:30:45.123456789 +0000 UTC",
			shouldError: false,
		},
		{
			name:        "invalid format - missing fields",
			input:       "2024-01-15 10:30:45",
			shouldError: true,
		},
		{
			name:        "invalid format - wrong separator",
			input:       "2024-01-15T10:30:45.123456789+0000UTC",
			shouldError: true,
		},
		{
			name:        "empty string",
			input:       "",
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parsingTimeFromString(ctx, tt.input, timestampFormat)
			if tt.shouldError {
				if err == nil {
					t.Errorf("parsingTimeFromString(%q) expected error but got nil", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("parsingTimeFromString(%q) unexpected error: %v", tt.input, err)
				}
				if result.IsZero() {
					t.Errorf("parsingTimeFromString(%q) returned zero time", tt.input)
				}
			}
		})
	}
}

func TestPrepareImage(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name             string
		config           SlurmConfig
		metadata         metav1.ObjectMeta
		containerImage   string
		expectedContains string
	}{
		{
			name: "image with default prefix",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "ubuntu:latest",
			expectedContains: "docker://ubuntu:latest",
		},
		{
			name: "image with custom prefix from annotation",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/image-root": "oras://",
				},
			},
			containerImage:   "myimage:v1",
			expectedContains: "oras://myimage:v1",
		},
		{
			name: "absolute path image",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "/path/to/image.sif",
			expectedContains: "/path/to/image.sif",
		},
		{
			name: "image already has prefix",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "docker://nginx:alpine",
			expectedContains: "docker://nginx:alpine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := prepareImage(ctx, tt.config, tt.metadata, tt.containerImage)
			if result != tt.expectedContains {
				t.Errorf("prepareImage() = %q, want %q", result, tt.expectedContains)
			}
		})
	}
}

func TestCheckIfJidExists(t *testing.T) {
	ctx := context.Background()
	jids := make(map[string]*JidStruct)

	// Add some test data
	jids["uid-1"] = &JidStruct{
		PodUID:       "uid-1",
		PodNamespace: "default",
		JID:          "12345",
		StartTime:    time.Now(),
	}

	tests := []struct {
		name     string
		uid      string
		expected bool
	}{
		{
			name:     "existing JID",
			uid:      "uid-1",
			expected: true,
		},
		{
			name:     "non-existing JID",
			uid:      "uid-2",
			expected: false,
		},
		{
			name:     "empty uid",
			uid:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checkIfJidExists(ctx, &jids, tt.uid)
			if result != tt.expected {
				t.Errorf("checkIfJidExists(%q) = %v, want %v", tt.uid, result, tt.expected)
			}
		})
	}
}

func TestRemoveJID(t *testing.T) {
	jids := make(map[string]*JidStruct)
	jids["uid-1"] = &JidStruct{
		PodUID:       "uid-1",
		PodNamespace: "default",
		JID:          "12345",
	}
	jids["uid-2"] = &JidStruct{
		PodUID:       "uid-2",
		PodNamespace: "default",
		JID:          "67890",
	}

	removeJID("uid-1", &jids)

	if _, exists := jids["uid-1"]; exists {
		t.Error("removeJID() failed to remove uid-1")
	}

	if _, exists := jids["uid-2"]; !exists {
		t.Error("removeJID() incorrectly removed uid-2")
	}
}
