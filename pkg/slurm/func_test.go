package slurm

import (
	"net/http"
	"net/http/httptest"
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
