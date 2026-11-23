package cloudcode

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// Google answers Cloud Code calls of an account it banned for a Terms of
// Service violation with a 403 like this one, captured from agy 1.5.3's
// traffic:
//
//	{"error": {"code": 403, "message": "This service has been disabled in this account for violation of Terms of Service. …",
//	  "status": "PERMISSION_DENIED",
//	  "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "TOS_VIOLATION",
//	    "metadata": {"appeal_url": "https://forms.gle/…", …}}]}}
const (
	permissionDenied = "PERMISSION_DENIED"
	tosViolation     = "TOS_VIOLATION"
)

// AccountDisabledFunc learns that Google banned the account; reason is
// Google's formatted error message, with the appeal link when it gave one.
type AccountDisabledFunc func(reason string)

func (ap *accountProxy) watchAccountDisabled(resp *http.Response) {
	if ap.onAccountDisabled == nil || resp.StatusCode != http.StatusForbidden {
		return
	}
	reason, ok := accountDisabledReason(resp)
	if !ok {
		return
	}
	ap.onAccountDisabled(reason)
}

// accountDisabledReason reads a TOS_VIOLATION refusal from resp, leaving its
// body for watchModelCall and agy as it was.
func accountDisabledReason(resp *http.Response) (string, bool) {
	raw, ok := readErrorBody(resp)
	if !ok {
		return "", false
	}

	var refusal struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Reason   string `json:"reason"`
				Metadata struct {
					AppealURL string `json:"appeal_url"`
				} `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &refusal) != nil || refusal.Error.Status != permissionDenied {
		return "", false
	}
	for _, d := range refusal.Error.Details {
		if d.Reason != tosViolation {
			continue
		}
		reason := describeError(resp.StatusCode, refusal.Error.Status, refusal.Error.Message)
		if u, err := url.Parse(d.Metadata.AppealURL); err == nil && u.Scheme == "https" && u.Host != "" {
			reason += " (appeal: " + d.Metadata.AppealURL + ")"
		}
		return reason, true
	}
	return "", false
}
