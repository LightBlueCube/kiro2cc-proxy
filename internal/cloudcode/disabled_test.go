package cloudcode

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

const tosRefusal = `{
  "error": {
    "code": 403,
    "message": "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {"appeal_url": "https://forms.gle/hGzM9MEUv2azZsrb9", "appeal_url_link_text": "Submit Appeal", "uiMessage": "true"}
      }
    ]
  }
}`

func TestWatchAccountDisabledReportsAndPreservesBody(t *testing.T) {
	var reasons []string
	ap := &accountProxy{onAccountDisabled: func(reason string) { reasons = append(reasons, reason) }}

	body := gzipped(t, tosRefusal)
	resp := response("/v1internal:streamGenerateContent", http.StatusForbidden, body, "gzip")
	ap.watchAccountDisabled(resp)

	want := "PERMISSION_DENIED (code 403): This service has been disabled in this account for violation of Terms of Service. " +
		"Please submit an appeal to continue using this product. (appeal: https://forms.gle/hGzM9MEUv2azZsrb9)"
	if len(reasons) != 1 || reasons[0] != want {
		t.Fatalf("reasons = %q, want [%q]", reasons, want)
	}

	// watchModelCall's callError and agy still read the intact response body.
	if ce := callError(resp); ce.Code != http.StatusForbidden {
		t.Fatalf("callError = %+v, want 403", ce)
	}
	if forwarded, _ := io.ReadAll(resp.Body); !bytes.Equal(forwarded, body) {
		t.Fatal("agy must get the body as Google sent it")
	}
}

func TestWatchAccountDisabledOmitsUnsafeAppealURL(t *testing.T) {
	var reasons []string
	ap := &accountProxy{onAccountDisabled: func(reason string) { reasons = append(reasons, reason) }}

	body := `{"error": {"code": 403, "message": "banned", "status": "PERMISSION_DENIED",
	  "details": [{"reason": "TOS_VIOLATION", "metadata": {"appeal_url": "javascript:alert(1)"}}]}}`
	ap.watchAccountDisabled(response("/v1internal:loadCodeAssist", http.StatusForbidden, []byte(body), ""))

	if want := "PERMISSION_DENIED (code 403): banned"; len(reasons) != 1 || reasons[0] != want {
		t.Fatalf("reasons = %q, want [%q]", reasons, want)
	}
}

func TestWatchAccountDisabledIgnoresOtherResponses(t *testing.T) {
	var reasons []string
	ap := &accountProxy{onAccountDisabled: func(reason string) { reasons = append(reasons, reason) }}

	verify := `{"error": {"code": 403, "message": "Verify your account to continue.", "status": "PERMISSION_DENIED",
	  "details": [{"reason": "VALIDATION_REQUIRED"}]}}`
	plain := `{"error": {"code": 403, "message": "The caller does not have permission", "status": "PERMISSION_DENIED"}}`
	notDenied := `{"error": {"code": 403, "message": "banned", "status": "FAILED_PRECONDITION", "details": [{"reason": "TOS_VIOLATION"}]}}`
	resp := response("/v1internal:streamGenerateContent", http.StatusForbidden, []byte(verify), "")
	ap.watchAccountDisabled(resp)
	ap.watchAccountDisabled(response("/v1internal:streamGenerateContent", http.StatusForbidden, []byte(plain), ""))
	ap.watchAccountDisabled(response("/v1internal:streamGenerateContent", http.StatusForbidden, []byte(notDenied), ""))
	ap.watchAccountDisabled(response("/v1internal:streamGenerateContent", http.StatusBadRequest, []byte(tosRefusal), ""))
	ap.watchAccountDisabled(response("/v1internal:fetchAvailableModels", http.StatusOK, []byte("{}"), ""))
	ap.watchAccountDisabled(response("/v1internal:streamGenerateContent", http.StatusForbidden, []byte(tosRefusal), "br"))

	if forwarded, _ := io.ReadAll(resp.Body); string(forwarded) != verify || len(reasons) != 0 {
		t.Fatalf("forwarded %q, reasons %q", forwarded, reasons)
	}
}
