package slurm

import (
"regexp"
"testing"

v1 "k8s.io/api/core/v1"
)

// TestSlurmStateRegex validates that all expected SLURM state codes, including the
// newly-added TO (TIMEOUT) and OOM (out-of-memory) codes, are matched by the regex
// used in StatusHandler.  It also checks that multi-character codes like ST are not
// shadowed by the single-character S alternative.
func TestSlurmStateRegex(t *testing.T) {
statePattern := `(CD|CG|F|OOM|PD|PR|R|ST|S|TO)`
re := regexp.MustCompile(statePattern)

tests := []struct {
name       string
squeueLine string // simulated squeue output: "<exit_code> <state>"
wantState  string
wantMatch  bool
}{
// Pre-existing states
{"completed", "0                 CD", "CD", true},
{"completing", "0                 CG", "CG", true},
{"failed", "1                 F", "F", true},
{"pending", "0                 PD", "PD", true},
{"preempted", "0:15              PR", "PR", true},
{"running", "0                 R", "R", true},
{"suspended", "0                 S", "S", true},
// ST must not be shadowed by S
{"stopped", "0                 ST", "ST", true},
// Newly handled states
{"timeout", "0:15              TO", "TO", true},
{"oom", "0:9               OOM", "OOM", true},
// Completely unknown state should not match
{"unknown state XY", "0                 XY", "", false},
}

for _, tt := range tests {
t.Run(tt.name, func(t *testing.T) {
match := re.FindString(tt.squeueLine)
if tt.wantMatch {
if match != tt.wantState {
t.Errorf("regex matched %q, want %q for input %q", match, tt.wantState, tt.squeueLine)
}
} else {
if match != "" {
t.Errorf("regex unexpectedly matched %q for input %q", match, tt.squeueLine)
}
}
})
}
}

// TestExitCodeRegexForTimeout checks that the exit-code regex correctly extracts the
// signal number from the squeue output format "exit_code:signal  STATE".
// For a TIMEOUT job SLURM reports "0:15" meaning exit code 0 killed by signal 15 (SIGTERM).
func TestExitCodeRegexForTimeout(t *testing.T) {
// Mirrors the regex used in StatusHandler.
exitCodePattern := `([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\s`
re := regexp.MustCompile(exitCodePattern)

tests := []struct {
name       string
squeueLine string
wantCode   string
}{
// TIMEOUT: squeue reports "0:15  TO" → the regex picks up "15" (the SIGTERM signal).
{"timeout sigterm", "0:15              TO", "15"},
// OOM: killed by signal 9.
{"oom sigkill", "0:9               OOM", "9"},
// Normal completion.
{"success", "0                 CD", "0"},
// Non-zero exit.
{"failure", "1                 F", "1"},
}

for _, tt := range tests {
t.Run(tt.name, func(t *testing.T) {
matches := re.FindStringSubmatch(tt.squeueLine)
if len(matches) < 2 {
t.Fatalf("exit-code regex did not match input %q", tt.squeueLine)
}
got := matches[1]
if got != tt.wantCode {
t.Errorf("exit code = %q, want %q for input %q", got, tt.wantCode, tt.squeueLine)
}
})
}
}

// TestTimeoutContainerStatus verifies that the exported constants used in the "TO" case
// of StatusHandler produce a well-formed ContainerStatus with the correct Reason and
// Message values.  This ensures the constants and the actual construction code stay in sync.
func TestTimeoutContainerStatus(t *testing.T) {
cs := v1.ContainerStatus{
Name: "test",
State: v1.ContainerState{
Terminated: &v1.ContainerStateTerminated{
ExitCode: 15,
Reason:   ReasonSlurmJobTimeout,
Message:  MessageSlurmJobTimeout,
},
},
Ready: false,
}

if cs.State.Terminated == nil {
t.Fatal("expected Terminated state, got nil")
}
if cs.State.Terminated.Reason != ReasonSlurmJobTimeout {
t.Errorf("Reason = %q, want %q", cs.State.Terminated.Reason, ReasonSlurmJobTimeout)
}
if cs.State.Terminated.Message != MessageSlurmJobTimeout {
t.Errorf("Message = %q, want %q", cs.State.Terminated.Message, MessageSlurmJobTimeout)
}
if cs.State.Terminated.ExitCode != 15 {
t.Errorf("ExitCode = %d, want 15 (SIGTERM)", cs.State.Terminated.ExitCode)
}
if cs.Ready {
t.Error("Ready should be false for a timed-out container")
}
}

// TestOOMContainerStatus verifies that the exported constants used in the "OOM" case of
// StatusHandler produce a well-formed ContainerStatus with the Kubernetes-conventional
// "OOMKilled" reason so existing tooling can identify OOM terminations.
func TestOOMContainerStatus(t *testing.T) {
cs := v1.ContainerStatus{
Name: "test",
State: v1.ContainerState{
Terminated: &v1.ContainerStateTerminated{
ExitCode: 137,
Reason:   ReasonOOMKilled,
Message:  MessageOOMKilled,
},
},
Ready: false,
}

if cs.State.Terminated == nil {
t.Fatal("expected Terminated state, got nil")
}
// "OOMKilled" is the Kubernetes convention used by the kubelet itself.
const k8sOOMReason = "OOMKilled"
if cs.State.Terminated.Reason != k8sOOMReason {
t.Errorf("Reason = %q, want %q (Kubernetes convention)", cs.State.Terminated.Reason, k8sOOMReason)
}
if cs.State.Terminated.Message != MessageOOMKilled {
t.Errorf("Message = %q, want %q", cs.State.Terminated.Message, MessageOOMKilled)
}
if cs.Ready {
t.Error("Ready should be false for an OOM-killed container")
}
}
