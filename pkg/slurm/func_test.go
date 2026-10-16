package slurm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetSessionContext(t *testing.T) {
	tests := []struct {
		name           string
		headerValue    string
		expectedResult string
	}{
		{
			name:           "header present",
			headerValue:    "session-123",
			expectedResult: "session-123",
		},
		{
			name:           "header empty",
			headerValue:    "",
			expectedResult: "NoSessionFound#0",
		},
		{
			name:           "header with special characters",
			headerValue:    "session@456#abc",
			expectedResult: "session@456#abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.headerValue != "" {
				req.Header.Set("InterLink-Http-Session", tt.headerValue)
			}

			result := GetSessionContext(req)
			if result != tt.expectedResult {
				t.Errorf("GetSessionContext() = %q, want %q", result, tt.expectedResult)
			}
		})
	}
}

func TestGetSessionContextMessage(t *testing.T) {
	tests := []struct {
		name           string
		sessionContext string
		expectedResult string
	}{
		{
			name:           "normal session",
			sessionContext: "session-123",
			expectedResult: "HTTP InterLink session session-123: ",
		},
		{
			name:           "empty session",
			sessionContext: "",
			expectedResult: "HTTP InterLink session : ",
		},
		{
			name:           "default no session",
			sessionContext: "NoSessionFound#0",
			expectedResult: "HTTP InterLink session NoSessionFound#0: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetSessionContextMessage(tt.sessionContext)
			if result != tt.expectedResult {
				t.Errorf("GetSessionContextMessage(%q) = %q, want %q", tt.sessionContext, result, tt.expectedResult)
			}
		})
	}
}

func TestSocketDirFor(t *testing.T) {
	tests := []struct {
		name string
		root string
		uid  string
		want string
	}{
		{name: "first 12 hex digits of the UID", root: socketTestRoot, uid: socketTestUID, want: socketTestDir},
		{name: "no root", uid: socketTestUID},
		{name: "UID too short", root: socketTestRoot, uid: socketTestShortUID},
		{name: "UID not hex", root: socketTestRoot, uid: "1b2c3d4e-5f6z-4711-8899-aabbccddeeff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := socketDirFor(tt.root, tt.uid); got != tt.want {
				t.Errorf("socketDirFor(%q, %q) = %q, want %q", tt.root, tt.uid, got, tt.want)
			}
		})
	}
}

// SocketDirRoot ends up in unix socket paths, which hold 107 bytes, and in shell
// and ssh arguments.
func TestValidateSocketDirRoot(t *testing.T) {
	longest := "/" + strings.Repeat("a", maxSocketDirRootLen-1)
	tests := []struct {
		name   string
		root   string
		want   string // empty: refused
		errHas string
	}{
		{name: "79 bytes", root: longest, want: longest},
		{name: "80 bytes", root: longest + "a", errHas: "at most 79 bytes"},
		{name: "cleaned", root: socketTestRoot + "//", want: socketTestRoot},
		{name: "relative", root: "shared/il"},
		{name: "dot-dot", root: "/shared/../il"},
		{name: "hidden element", root: "/shared/.il"},
		{name: "space", root: "/shared/i l"},
		{name: "dollar", root: "/shared/$USER"},
		{name: "filesystem root", root: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateSocketDirRoot(tt.root)
			if tt.want == "" {
				if err == nil || !strings.Contains(err.Error(), tt.errHas) {
					t.Errorf("validateSocketDirRoot(%q) = %q, %v; want an error containing %q", tt.root, got, err, tt.errHas)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("validateSocketDirRoot(%q) = %q, %v; want %q", tt.root, got, err, tt.want)
			}
		})
	}
}
