package cloudcode

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Google answers Cloud Code calls sent from an unsupported region with a 400
// FAILED_PRECONDITION like:
//
//	{"error": {"code": 400, "message": "User location is not supported for the API use.", "status": "FAILED_PRECONDITION"}}
const (
	failedPrecondition        = "FAILED_PRECONDITION"
	locationUnsupportedPhrase = "user location is not supported"
)

// LocationUnsupportedFunc learns that Google rejected a Cloud Code call
// because the account's exit location is unsupported; reason is Google's
// formatted error message.
type LocationUnsupportedFunc func(reason string)

func (ap *accountProxy) watchUnsupportedLocation(resp *http.Response) {
	if ap.onLocationUnsupported == nil || resp.StatusCode != http.StatusBadRequest {
		return
	}
	reason, ok := unsupportedLocationReason(resp)
	if !ok {
		return
	}
	ap.onLocationUnsupported(reason)
}

// unsupportedLocationReason reads a FAILED_PRECONDITION unsupported-location
// refusal from resp, leaving its body for watchModelCall and agy as it was.
func unsupportedLocationReason(resp *http.Response) (string, bool) {
	raw, ok := readErrorBody(resp)
	if !ok {
		return "", false
	}

	var refusal struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &refusal) != nil {
		return "", false
	}
	isUnsupported := refusal.Error.Status == failedPrecondition &&
		strings.Contains(strings.ToLower(refusal.Error.Message), locationUnsupportedPhrase)
	if !isUnsupported {
		return "", false
	}
	return describeError(resp.StatusCode, refusal.Error.Status, refusal.Error.Message), true
}
