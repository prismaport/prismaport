package service

import (
	"net/http"
	"testing"
)

// The statuses kept with stored responses are net/http's: a replay answers what the first
// request answered.
func TestStoredResponseStatusesAreHTTPs(t *testing.T) {
	if statusOK != http.StatusOK || statusCreated != http.StatusCreated {
		t.Fatalf("stored statuses %d %d", statusOK, statusCreated)
	}
}
