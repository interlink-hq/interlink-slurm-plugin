package slurm

import (
	"encoding/json"
	"testing"
)

// TestGetClusterResourcesFromText_HappyPath verifies that the text-parsing path
// correctly aggregates per-node CPU and memory fields from a multi-line sinfo output.
func TestGetClusterResourcesFromText_HappyPath(t *testing.T) {
	// Simulate what sinfo --noheader --format=%c|%m|%e would return for two nodes:
	//   node01: 16 CPUs, 128 000 MB total, 64 000 MB free  → 64 000 MB used
	//   node02: 32 CPUs,  64 000 MB total, 32 000 MB free  → 32 000 MB used
	sinfoLines := "16|128000|64000\n32|64000|32000\n"

	resources, err := parseClusterResourcesFromText(sinfoLines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resources.CPUTotalCores != 48 {
		t.Errorf("CPUTotalCores = %d, want 48", resources.CPUTotalCores)
	}
	// Text mode cannot determine allocated CPUs.
	if resources.CPUUsedCores != 0 {
		t.Errorf("CPUUsedCores = %d, want 0 (not derivable from text output)", resources.CPUUsedCores)
	}

	wantTotalMem := int64(192000) * 1024 * 1024
	if resources.MemoryTotalBytes != wantTotalMem {
		t.Errorf("MemoryTotalBytes = %d, want %d", resources.MemoryTotalBytes, wantTotalMem)
	}

	wantUsedMem := int64(96000) * 1024 * 1024
	if resources.MemoryUsedBytes != wantUsedMem {
		t.Errorf("MemoryUsedBytes = %d, want %d", resources.MemoryUsedBytes, wantUsedMem)
	}
}

// TestGetClusterResourcesFromText_SkipsInvalidLines verifies that lines that cannot
// be parsed (wrong field count, non-integer values) are silently skipped so that
// partial data does not cause a hard failure.
func TestGetClusterResourcesFromText_SkipsInvalidLines(t *testing.T) {
	// Mix valid and invalid lines
	sinfoLines := "8|32000|16000\nbadline\n4|N/A|8000\n4|16000|8000\n"

	resources, err := parseClusterResourcesFromText(sinfoLines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the first and last valid lines should contribute
	if resources.CPUTotalCores != 12 {
		t.Errorf("CPUTotalCores = %d, want 12", resources.CPUTotalCores)
	}
	wantTotalMem := int64(48000) * 1024 * 1024
	if resources.MemoryTotalBytes != wantTotalMem {
		t.Errorf("MemoryTotalBytes = %d, want %d", resources.MemoryTotalBytes, wantTotalMem)
	}
}

// TestGetClusterResourcesFromText_EmptyOutput verifies that an empty sinfo output
// returns a zero-value NodeResources without error.
func TestGetClusterResourcesFromText_EmptyOutput(t *testing.T) {
	resources, err := parseClusterResourcesFromText("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resources.CPUTotalCores != 0 || resources.MemoryTotalBytes != 0 {
		t.Errorf("expected zero resources for empty input, got %+v", resources)
	}
}

// TestGetClusterResourcesFromJSON_HappyPath verifies that the JSON-parsing path
// correctly aggregates per-node data from a `sinfo --json` response.
func TestGetClusterResourcesFromJSON_HappyPath(t *testing.T) {
	// Simulate a minimal `sinfo --json` response with two nodes.
	nodes := slurmNodeList{
		Nodes: []slurmJSONNode{
			{CPUs: 16, AllocCPUs: 8, RealMemory: 128000, FreeMemory: 64000, AllocMemory: 64000},
			{CPUs: 32, AllocCPUs: 16, RealMemory: 64000, FreeMemory: 32000, AllocMemory: 32000},
		},
	}
	jsonBytes, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("failed to marshal test data: %v", err)
	}

	resources, err := parseClusterResourcesFromJSON(string(jsonBytes))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resources.CPUTotalCores != 48 {
		t.Errorf("CPUTotalCores = %d, want 48", resources.CPUTotalCores)
	}
	if resources.CPUUsedCores != 24 {
		t.Errorf("CPUUsedCores = %d, want 24", resources.CPUUsedCores)
	}

	wantTotalMem := int64(192000) * 1024 * 1024
	if resources.MemoryTotalBytes != wantTotalMem {
		t.Errorf("MemoryTotalBytes = %d, want %d", resources.MemoryTotalBytes, wantTotalMem)
	}

	wantUsedMem := int64(96000) * 1024 * 1024
	if resources.MemoryUsedBytes != wantUsedMem {
		t.Errorf("MemoryUsedBytes = %d, want %d", resources.MemoryUsedBytes, wantUsedMem)
	}
}

// TestGetClusterResourcesFromJSON_FallbackToFreeMemory verifies that when
// alloc_memory is zero the implementation falls back to real_memory - free_memory.
func TestGetClusterResourcesFromJSON_FallbackToFreeMemory(t *testing.T) {
	nodes := slurmNodeList{
		Nodes: []slurmJSONNode{
			// alloc_memory not set (0) → used = real_memory - free_memory
			{CPUs: 8, AllocCPUs: 4, RealMemory: 32000, FreeMemory: 16000, AllocMemory: 0},
		},
	}
	jsonBytes, _ := json.Marshal(nodes)

	resources, err := parseClusterResourcesFromJSON(string(jsonBytes))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantUsedMem := int64(16000) * 1024 * 1024
	if resources.MemoryUsedBytes != wantUsedMem {
		t.Errorf("MemoryUsedBytes = %d, want %d (fallback to real-free)",
			resources.MemoryUsedBytes, wantUsedMem)
	}
}

// TestGetClusterResourcesFromJSON_EmptyNodes verifies that a JSON response with an
// empty node list is treated as an error, triggering the text fallback.
func TestGetClusterResourcesFromJSON_EmptyNodes(t *testing.T) {
	nodes := slurmNodeList{Nodes: []slurmJSONNode{}}
	jsonBytes, _ := json.Marshal(nodes)

	_, err := parseClusterResourcesFromJSON(string(jsonBytes))
	if err == nil {
		t.Error("expected an error for empty nodes list, got nil")
	}
}

// TestGetClusterResourcesFromJSON_InvalidJSON verifies that malformed JSON causes
// an error so the handler can fall back to text parsing.
func TestGetClusterResourcesFromJSON_InvalidJSON(t *testing.T) {
	_, err := parseClusterResourcesFromJSON("not valid json")
	if err == nil {
		t.Error("expected an error for invalid JSON, got nil")
	}
}

// TestNodeResourcesJSONSerialization verifies that NodeResources serialises to the
// expected JSON keys so consumers can reliably decode the ping response.
func TestNodeResourcesJSONSerialization(t *testing.T) {
	nr := NodeResources{
		CPUTotalCores:    100,
		CPUUsedCores:     50,
		MemoryTotalBytes: 1073741824,
		MemoryUsedBytes:  536870912,
		MaxPods:          1000,
	}

	b, err := json.Marshal(nr)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	for _, key := range []string{"cpu_total_cores", "cpu_used_cores", "memory_total_bytes", "memory_used_bytes", "max_pods"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected JSON key %q not found in output", key)
		}
	}
}

// TestNodeResourcesJSONOmitEmptyMaxPods verifies that MaxPods is omitted when zero
// so that consumers that do not expect it are not confused.
func TestNodeResourcesJSONOmitEmptyMaxPods(t *testing.T) {
	nr := NodeResources{
		CPUTotalCores:    10,
		MemoryTotalBytes: 1024,
	}

	b, _ := json.Marshal(nr)
	if json.Valid(b) {
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		if _, ok := m["max_pods"]; ok {
			t.Error("max_pods should be omitted when zero")
		}
	}
}
