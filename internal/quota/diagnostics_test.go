package quota

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnosticErrorRetainsHTTPStatusWithoutBody(t *testing.T) {
	err := fmt.Errorf("private-response-body: %w", &httpError{status: 401})
	if DiagnosticErrorCode(err) != "http_401" {
		t.Fatal("HTTP status lost")
	}
	if strings.Contains(DiagnosticErrorCode(errors.New("private-token")), "private-token") {
		t.Fatal("arbitrary error leaked")
	}
	if DiagnosticErrorCode(errors.New("HTTP 429 (retry in 30s)")) != "http_429" {
		t.Fatal("cached cooldown status lost")
	}
}
