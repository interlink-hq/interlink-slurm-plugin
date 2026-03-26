package slurm

import (
	"regexp"
	"testing"
	"time"
)

// TestSlurmStateRegex validates that all expected SLURM state codes, including the
// newly-added TO (TIMEOUT) and OOM (out-of-memory) codes, are matched by the regex
// used in StatusHandler.  It also checks that multi-character codes like ST are not
// shadowed by the single-character S alternative.
func TestSlurmStateRegex(t *testing.T) {
	re := regexp.MustCompile(SlurmStatePattern)

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
	re := regexp.MustCompile(SlurmExitCodePattern)

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

// TestTerminatedContainerStatusTO verifies that terminatedContainerStatus, the helper
// used in the "TO" case of StatusHandler, produces a ContainerStatus with
// Reason=ReasonSlurmJobTimeout and Message=MessageSlurmJobTimeout.
func TestTerminatedContainerStatusTO(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("test", start, finish, 15, ReasonSlurmJobTimeout, MessageSlurmJobTimeout)

	if cs.Name != "test" {
		t.Errorf("Name = %q, want %q", cs.Name, "test")
	}
	if cs.Ready {
		t.Error("Ready should be false")
	}
	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	if term.Reason != ReasonSlurmJobTimeout {
		t.Errorf("Reason = %q, want %q", term.Reason, ReasonSlurmJobTimeout)
	}
	if term.Message != MessageSlurmJobTimeout {
		t.Errorf("Message = %q, want %q", term.Message, MessageSlurmJobTimeout)
	}
	if term.ExitCode != 15 {
		t.Errorf("ExitCode = %d, want 15", term.ExitCode)
	}
	if !term.StartedAt.Time.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", term.StartedAt.Time, start)
	}
	if !term.FinishedAt.Time.Equal(finish) {
		t.Errorf("FinishedAt = %v, want %v", term.FinishedAt.Time, finish)
	}
}

// TestTerminatedContainerStatusOOM verifies that terminatedContainerStatus, the helper
// used in the "OOM" case of StatusHandler, produces a ContainerStatus with
// Reason=ReasonOOMKilled (the Kubernetes convention) and Message=MessageOOMKilled.
func TestTerminatedContainerStatusOOM(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("mycontainer", start, finish, 9, ReasonOOMKilled, MessageOOMKilled)

	if cs.Name != "mycontainer" {
		t.Errorf("Name = %q, want %q", cs.Name, "mycontainer")
	}
	if cs.Ready {
		t.Error("Ready should be false")
	}
	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	// "OOMKilled" is the Kubernetes convention used by the kubelet itself.
	if term.Reason != "OOMKilled" {
		t.Errorf("Reason = %q, want %q (Kubernetes convention)", term.Reason, "OOMKilled")
	}
	if term.Message != MessageOOMKilled {
		t.Errorf("Message = %q, want %q", term.Message, MessageOOMKilled)
	}
	if term.ExitCode != 9 {
		t.Errorf("ExitCode = %d, want 9", term.ExitCode)
	}
	if !term.StartedAt.Time.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", term.StartedAt.Time, start)
	}
	if !term.FinishedAt.Time.Equal(finish) {
		t.Errorf("FinishedAt = %v, want %v", term.FinishedAt.Time, finish)
	}
}

// TestTerminatedContainerStatusNoReason verifies that terminatedContainerStatus works
// for ordinary termination cases (CD/F/ST/default) where no named reason is needed.
func TestTerminatedContainerStatusNoReason(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("worker", start, finish, 0, "", "")

	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	if term.Reason != "" {
		t.Errorf("Reason = %q, want empty string for ordinary termination", term.Reason)
	}
	if term.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", term.ExitCode)
	}
}
