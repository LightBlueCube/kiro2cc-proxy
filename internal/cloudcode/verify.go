package cloudcode

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// Google answers Cloud Code calls of an account its owner must verify
// first (phone or QR check in a browser) with a 403 like this one, posted
// in lbjlaq/Antigravity-Manager#2362 and seen in agy 1.5.3's worker log:
//
//	{"error": {"code": 403, "message": "Verify your account to continue.", "status": "PERMISSION_DENIED",
//	  "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "VALIDATION_REQUIRED",
//	    "metadata": {"validation_url": "https://accounts.google.com/signin/continue?…", …}}, …]}}
//
// agy's language server passes on only the message, so the proxy reads
// the link from the response it forwards.
const validationRequired = "VALIDATION_REQUIRED"

// maxErrorBytes bounds the error bodies read; longer ones pass unread.
const maxErrorBytes = 64 << 10

// Verification is Google's request that the account's owner verify the
// account before it serves.
type Verification struct {
	// URL is Google's page for the check, "" when it named none.
	URL string
}

// VerificationFunc learns the account's verification state: non-nil when
// Google asks for one, nil once every call it refused succeeded since.
type VerificationFunc func(*Verification)

// watchVerification tracks, per account, the calls (paths) Google refused
// for a missing verification. It reports each refusal, and nil once each
// refused call succeeded.
func (s *Server) watchVerification(account string, resp *http.Response) {
	path := resp.Request.URL.Path
	switch {
	case resp.StatusCode == http.StatusForbidden:
		link, ok := verificationLink(resp)
		if !ok {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.refused[account] == nil {
			s.refused[account] = map[string]string{}
		}
		s.refused[account][path] = link
		s.reportLocked(account, &Verification{URL: link})
	case resp.StatusCode/100 == 2:
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.refused[account][path]; !ok {
			return
		}
		delete(s.refused[account], path)
		if len(s.refused[account]) == 0 {
			s.reportLocked(account, nil)
		}
	}
}

// ClearVerification forgets the calls refused so far, as after a new
// sign-in or before a recheck: the next refusal reports again.
func (s *Server) ClearVerification(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refused[account]) == 0 {
		return
	}
	delete(s.refused, account)
	s.reportLocked(account, nil)
}

// reportLocked tells the account's watcher; under s.mu, so reports keep
// their order.
func (s *Server) reportLocked(account string, v *Verification) {
	if ap := s.accounts[account]; ap != nil && ap.onVerification != nil {
		ap.onVerification(v)
	}
}

// verificationLink reads a VALIDATION_REQUIRED refusal from resp, leaving
// its body for agy as it was. The link is kept only when it is https.
func verificationLink(resp *http.Response) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+1))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
	if err != nil || len(raw) > maxErrorBytes {
		return "", false
	}

	var body io.Reader = bytes.NewReader(raw)
	switch resp.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		unzipped, err := gzip.NewReader(body)
		if err != nil {
			return "", false
		}
		body = io.LimitReader(unzipped, maxErrorBytes)
	default:
		return "", false
	}

	var refusal struct {
		Error struct {
			Details []struct {
				Reason   string `json:"reason"`
				Metadata struct {
					ValidationURL string `json:"validation_url"`
				} `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.NewDecoder(body).Decode(&refusal) != nil {
		return "", false
	}
	for _, d := range refusal.Error.Details {
		if d.Reason != validationRequired {
			continue
		}
		link := d.Metadata.ValidationURL
		if u, err := url.Parse(link); err != nil || u.Scheme != "https" || u.Host == "" {
			link = ""
		}
		return link, true
	}
	return "", false
}
