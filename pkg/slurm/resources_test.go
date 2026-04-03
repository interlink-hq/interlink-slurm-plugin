package slurm

import (
"encoding/json"
"testing"
)

// TestGetClusterResourcesFromText_HappyPath verifies that the text-parsing path
// correctly aggregates per-node CPU and memory fields from a multi-line sinfo output
// and returns a PingResponse aligned with interlink-hq/interLink#516.
func TestGetClusterResourcesFromText_HappyPath(t *testing.T) {
// Simulate what sinfo --noheader --format=%c|%m|%e would return for two nodes:
//   node01: 16 CPUs, 128 000 MB total, 64 000 MB free
//   node02: 32 CPUs,  64 000 MB total, 32 000 MB free
sinfoLines := "16|128000|64000\n32|64000|32000\n"

resp, err := parseClusterResourcesFromText(sinfoLines)
if err != nil {
t.Fatalf("unexpected error: %v", err)
}

if resp.Status != "ok" {
t.Errorf("Status = %q, want %q", resp.Status, "ok")
}
if resp.Resources == nil {
t.Fatal("Resources is nil, want non-nil")
}

// In text mode total CPUs are reported (allocated count is not derivable).
if resp.Resources.CPU != "48" {
t.Errorf("CPU = %q, want %q", resp.Resources.CPU, "48")
}

// Memory should be the total free across all nodes (64000 + 32000 = 96000 Mi).
if resp.Resources.Memory != "96000Mi" {
t.Errorf("Memory = %q, want %q", resp.Resources.Memory, "96000Mi")
}
}

// TestGetClusterResourcesFromText_SkipsInvalidLines verifies that lines that cannot
// be parsed (wrong field count, non-integer values) are silently skipped so that
// partial data does not cause a hard failure.
func TestGetClusterResourcesFromText_SkipsInvalidLines(t *testing.T) {
// Mix valid and invalid lines
sinfoLines := "8|32000|16000\nbadline\n4|N/A|8000\n4|16000|8000\n"

resp, err := parseClusterResourcesFromText(sinfoLines)
if err != nil {
t.Fatalf("unexpected error: %v", err)
}

// Only the first (8 CPUs) and last (4 CPUs) valid lines contribute.
if resp.Resources == nil {
t.Fatal("Resources is nil")
}
if resp.Resources.CPU != "12" {
t.Errorf("CPU = %q, want %q", resp.Resources.CPU, "12")
}
// Free memory: 16000 (first) + 8000 (last) = 24000 Mi.
if resp.Resources.Memory != "24000Mi" {
t.Errorf("Memory = %q, want %q", resp.Resources.Memory, "24000Mi")
}
}

// TestGetClusterResourcesFromText_EmptyOutput verifies that an empty sinfo output
// returns a zero-value PingResponse with status "ok" and no error.
func TestGetClusterResourcesFromText_EmptyOutput(t *testing.T) {
resp, err := parseClusterResourcesFromText("")
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if resp.Status != "ok" {
t.Errorf("Status = %q, want %q", resp.Status, "ok")
}
if resp.Resources == nil {
t.Fatal("Resources is nil")
}
if resp.Resources.CPU != "0" {
t.Errorf("CPU = %q, want %q (zero CPU for empty output)", resp.Resources.CPU, "0")
}
if resp.Resources.Memory != "0Mi" {
t.Errorf("Memory = %q, want %q (zero memory for empty output)", resp.Resources.Memory, "0Mi")
}
}

// TestGetClusterResourcesFromJSON_HappyPath verifies that the JSON-parsing path
// correctly aggregates per-node data from a `sinfo --json` response and returns
// a PingResponse with Kubernetes-quantity strings for available resources.
func TestGetClusterResourcesFromJSON_HappyPath(t *testing.T) {
// Two nodes, each with half their CPUs and memory allocated.
//   node01: 16 CPUs (8 used), 128 000 MB total, 64 000 MB used
//   node02: 32 CPUs (16 used), 64 000 MB total, 32 000 MB used
// Available: 24 CPUs, 96 000 MB
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

resp, err := parseClusterResourcesFromJSON(string(jsonBytes))
if err != nil {
t.Fatalf("unexpected error: %v", err)
}

if resp.Status != "ok" {
t.Errorf("Status = %q, want %q", resp.Status, "ok")
}
if resp.Resources == nil {
t.Fatal("Resources is nil")
}
// Available CPU = 48 total - 24 used = 24
if resp.Resources.CPU != "24" {
t.Errorf("CPU = %q, want %q", resp.Resources.CPU, "24")
}
// Available memory = 192 000 MB total - 96 000 MB used = 96 000 Mi
if resp.Resources.Memory != "96000Mi" {
t.Errorf("Memory = %q, want %q", resp.Resources.Memory, "96000Mi")
}
}

// TestGetClusterResourcesFromJSON_FallbackToFreeMemory verifies that when
// alloc_memory is zero the implementation falls back to real_memory - free_memory
// to compute the used memory.
func TestGetClusterResourcesFromJSON_FallbackToFreeMemory(t *testing.T) {
nodes := slurmNodeList{
Nodes: []slurmJSONNode{
// alloc_memory not set (0) → used = real_memory(32000) - free_memory(16000) = 16000 MB
{CPUs: 8, AllocCPUs: 4, RealMemory: 32000, FreeMemory: 16000, AllocMemory: 0},
},
}
jsonBytes, _ := json.Marshal(nodes)

resp, err := parseClusterResourcesFromJSON(string(jsonBytes))
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if resp.Resources == nil {
t.Fatal("Resources is nil")
}
// Available CPU = 8 - 4 = 4; available memory = 32000 - 16000 = 16000 Mi.
if resp.Resources.CPU != "4" {
t.Errorf("CPU = %q, want %q", resp.Resources.CPU, "4")
}
if resp.Resources.Memory != "16000Mi" {
t.Errorf("Memory = %q, want %q (fallback to real-free)", resp.Resources.Memory, "16000Mi")
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

// TestPingResponseJSONSerialization verifies that PingResponse serialises to the
// exact JSON schema expected by interlink-hq/interLink#516 so the VK can unmarshal
// it and call updateNodeResources().
func TestPingResponseJSONSerialization(t *testing.T) {
resp := PingResponse{
Status: "ok",
Resources: &ResourcesResponse{
CPU:    "128",
Memory: "512Gi",
Pods:   "1000",
Accelerators: []AcceleratorResponse{
{ResourceType: "nvidia.com/gpu", Available: "8"},
},
},
}

b, err := json.Marshal(resp)
if err != nil {
t.Fatalf("json.Marshal failed: %v", err)
}

var m map[string]interface{}
if err := json.Unmarshal(b, &m); err != nil {
t.Fatalf("json.Unmarshal failed: %v", err)
}

for _, key := range []string{"status", "resources"} {
if _, ok := m[key]; !ok {
t.Errorf("expected JSON key %q not found in output", key)
}
}

resourcesMap, ok := m["resources"].(map[string]interface{})
if !ok {
t.Fatal("resources field is not a JSON object")
}
for _, key := range []string{"cpu", "memory", "pods", "accelerators"} {
if _, ok := resourcesMap[key]; !ok {
t.Errorf("expected resources.%q not found in output", key)
}
}
}

// TestPingResponseJSONOmitEmptyResources verifies that the resources field is
// omitted when nil, keeping the response compact for status-only replies.
func TestPingResponseJSONOmitEmptyResources(t *testing.T) {
resp := PingResponse{Status: "ok"}

b, _ := json.Marshal(resp)
var m map[string]interface{}
json.Unmarshal(b, &m)
if _, ok := m["resources"]; ok {
t.Error("resources field should be omitted when nil")
}
}

// TestPingResponseRoundTrip verifies that a PingResponse survives a marshal/unmarshal
// round-trip with all fields intact.
func TestPingResponseRoundTrip(t *testing.T) {
original := PingResponse{
Status: "ok",
Resources: &ResourcesResponse{
CPU:    "64",
Memory: "256Gi",
Pods:   "500",
Accelerators: []AcceleratorResponse{
{ResourceType: "nvidia.com/gpu", Available: "4"},
},
},
}

b, err := json.Marshal(original)
if err != nil {
t.Fatalf("marshal failed: %v", err)
}

var decoded PingResponse
if err := json.Unmarshal(b, &decoded); err != nil {
t.Fatalf("unmarshal failed: %v", err)
}

if decoded.Status != original.Status {
t.Errorf("Status = %q, want %q", decoded.Status, original.Status)
}
if decoded.Resources == nil {
t.Fatal("decoded Resources is nil")
}
if decoded.Resources.CPU != original.Resources.CPU {
t.Errorf("Resources.CPU = %q, want %q", decoded.Resources.CPU, original.Resources.CPU)
}
if decoded.Resources.Memory != original.Resources.Memory {
t.Errorf("Resources.Memory = %q, want %q", decoded.Resources.Memory, original.Resources.Memory)
}
if len(decoded.Resources.Accelerators) != 1 ||
decoded.Resources.Accelerators[0].ResourceType != "nvidia.com/gpu" ||
decoded.Resources.Accelerators[0].Available != "4" {
t.Errorf("Accelerators mismatch: got %+v", decoded.Resources.Accelerators)
}
}
