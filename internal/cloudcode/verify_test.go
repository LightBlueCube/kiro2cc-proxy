package cloudcode

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// refusal is the 403 body of lbjlaq/Antigravity-Manager#2362, plt made up.
const refusal = `{"error": {"code": 403, "message": "Verify your account to continue.", "status": "PERMISSION_DENIED",
  "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "VALIDATION_REQUIRED", "domain": "cloudcode-pa.googleapis.com",
    "metadata": {"validation_error_message": "Verify your account to continue.", "validation_url_link_text": "Verify your account",
      "validation_url": "https://accounts.google.com/signin/continue?sarp=1&plt=FAKE&flowName=GlifWebSignIn&authuser"}},
    {"@type": "type.googleapis.com/google.rpc.Help", "links": [{"description": "Verify your account", "url": "https://accounts.google.com/x"}]}]}}`

const verifyLink = "https://accounts.google.com/signin/continue?sarp=1&plt=FAKE&flowName=GlifWebSignIn&authuser"

func response(path string, status int, body []byte, encoding string) *http.Response {
	resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)),
		Request: &http.Request{URL: &url.URL{Path: path}}}
	if encoding != "" {
		resp.Header.Set("Content-Encoding", encoding)
	}
	return resp
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// watched is a Server with account "main", recording what it reports.
func watched() (*Server, *[]*Verification) {
	reports := &[]*Verification{}
	s := &Server{accounts: map[string]*accountProxy{}, refused: map[string]map[string]string{}}
	s.accounts["main"] = &accountProxy{onVerification: func(v *Verification) { *reports = append(*reports, v) }}
	return s, reports
}

func TestWatchVerificationReportsUntilEveryRefusedCallSucceeds(t *testing.T) {
	s, reports := watched()
	body := gzipped(t, refusal)
	resp := response("/v1internal:loadCodeAssist", http.StatusForbidden, body, "gzip")
	s.watchVerification("main", resp)
	if forwarded, _ := io.ReadAll(resp.Body); !bytes.Equal(forwarded, body) {
		t.Fatal("agy must get the body as Google sent it")
	}
	s.watchVerification("main", response("/v1internal:retrieveUserQuotaSummary", http.StatusForbidden, []byte(refusal), ""))
	if len(*reports) != 2 || (*reports)[0] == nil || (*reports)[0].URL != verifyLink {
		t.Fatalf("reports = %+v", *reports)
	}

	s.watchVerification("main", response("/v1internal:loadCodeAssist", http.StatusOK, []byte("{}"), ""))
	s.watchVerification("main", response("/v1internal:fetchUserInfo", http.StatusOK, []byte("{}"), ""))
	if len(*reports) != 2 {
		t.Fatalf("the quota call is still refused: %+v", *reports)
	}
	s.watchVerification("main", response("/v1internal:retrieveUserQuotaSummary", http.StatusOK, []byte("{}"), ""))
	if len(*reports) != 3 || (*reports)[2] != nil {
		t.Fatalf("reports = %+v", *reports)
	}
}

func TestWatchVerificationIgnoresOtherRefusals(t *testing.T) {
	s, reports := watched()
	other := `{"error": {"code": 403, "message": "The caller does not have permission", "status": "PERMISSION_DENIED"}}`
	resp := response("/v1internal:loadCodeAssist", http.StatusForbidden, []byte(other), "")
	s.watchVerification("main", resp)
	s.watchVerification("main", response("/v1internal:loadCodeAssist", http.StatusForbidden, []byte(refusal), "br"))
	s.watchVerification("main", response("/v1internal:loadCodeAssist", http.StatusForbidden, []byte(strings.Repeat(" ", maxErrorBytes)+refusal), ""))
	if forwarded, _ := io.ReadAll(resp.Body); string(forwarded) != other || len(*reports) != 0 {
		t.Fatalf("forwarded %q, reports %+v", forwarded, *reports)
	}
}

func TestWatchVerificationKeepsOnlyHTTPSLinks(t *testing.T) {
	s, reports := watched()
	body := strings.Replace(refusal, verifyLink, "javascript:alert(1)", 1)
	s.watchVerification("main", response("/v1internal:loadCodeAssist", http.StatusForbidden, []byte(body), ""))
	if len(*reports) != 1 || (*reports)[0] == nil || (*reports)[0].URL != "" {
		t.Fatalf("reports = %+v", *reports)
	}

	s.ClearVerification("main")
	s.ClearVerification("main")
	if len(*reports) != 2 || (*reports)[1] != nil {
		t.Fatalf("a clear reports once: %+v", *reports)
	}
}
