package cloudcode

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

const locationRefusal = `{"error": {"code": 400, "message": "User location is not supported for the API use.", "status": "FAILED_PRECONDITION"}}`

func TestWatchUnsupportedLocationReportsAndPreservesBody(t *testing.T) {
	var reasons []string
	ap := &accountProxy{onLocationUnsupported: func(reason string) { reasons = append(reasons, reason) }}

	body := gzipped(t, locationRefusal)
	resp := response("/v1internal:streamGenerateContent", http.StatusBadRequest, body, "gzip")
	ap.watchUnsupportedLocation(resp)

	want := "FAILED_PRECONDITION (code 400): User location is not supported for the API use."
	if len(reasons) != 1 || reasons[0] != want {
		t.Fatalf("reasons = %q, want [%q]", reasons, want)
	}

	// watchModelCall's callError and agy still read the intact response body.
	ce := callError(resp)
	if ce.Code != http.StatusBadRequest || ce.Message != want {
		t.Fatalf("callError = %+v, want 400 %q", ce, want)
	}
	if forwarded, _ := io.ReadAll(resp.Body); !bytes.Equal(forwarded, body) {
		t.Fatal("agy must get the body as Google sent it")
	}
}

func TestWatchUnsupportedLocationIgnoresOtherResponses(t *testing.T) {
	var reasons []string
	ap := &accountProxy{onLocationUnsupported: func(reason string) { reasons = append(reasons, reason) }}

	other400 := `{"error": {"code": 400, "message": "Request contains an invalid argument.", "status": "INVALID_ARGUMENT"}}`
	otherPrecond := `{"error": {"code": 400, "message": "Billing account is closed.", "status": "FAILED_PRECONDITION"}}`
	resp := response("/v1internal:streamGenerateContent", http.StatusBadRequest, []byte(other400), "")
	ap.watchUnsupportedLocation(resp)
	ap.watchUnsupportedLocation(response("/v1internal:streamGenerateContent", http.StatusBadRequest, []byte(otherPrecond), ""))
	ap.watchUnsupportedLocation(response("/v1internal:streamGenerateContent", http.StatusForbidden, []byte(locationRefusal), ""))
	ap.watchUnsupportedLocation(response("/v1internal:fetchAvailableModels", http.StatusOK, []byte("{}"), ""))
	ap.watchUnsupportedLocation(response("/v1internal:streamGenerateContent", http.StatusBadRequest, []byte(locationRefusal), "br"))
	ap.watchUnsupportedLocation(response("/v1internal:streamGenerateContent", http.StatusBadRequest, []byte(strings.Repeat(" ", maxErrorBytes)+locationRefusal), ""))

	if forwarded, _ := io.ReadAll(resp.Body); string(forwarded) != other400 || len(reasons) != 0 {
		t.Fatalf("forwarded %q, reasons %q", forwarded, reasons)
	}
}
