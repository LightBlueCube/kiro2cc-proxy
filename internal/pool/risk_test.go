package pool

import (
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

const riskModel = "gemini-3.8-flash"

var (
	noWait429 = ModelFailure{Code: http.StatusTooManyRequests, Message: "RESOURCE_EXHAUSTED (code 429): Resource has been exhausted (e.g. check quota)."}
	quota429  = ModelFailure{Code: http.StatusTooManyRequests, Message: "quota", Reset: time.Now().Add(time.Hour)}
)

func riskAccount(t *testing.T) *Account {
	t.Helper()
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Add(AccountConfig{Name: "a", Home: filepath.Join(t.TempDir(), "a")})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRiskWhenPeersServeAndSecondNoWait429Arrives(t *testing.T) {
	a := riskAccount(t)
	if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
		t.Fatal("risk on the first 429")
	}
	a.RecordModelCall("S2", riskModel, nil)
	if a.RecordModelCall("S1", riskModel, &noWait429) != CallRisky {
		t.Fatal("no risk on the second 429 with a peer served")
	}
	if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
		t.Fatal("risk reported twice for one turn")
	}
}

func TestRiskWithoutPeersWaitsForThirdNoWait429(t *testing.T) {
	a := riskAccount(t)
	for i := 1; i <= 2; i++ {
		if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
			t.Fatalf("risk on 429 %d with nobody else calling", i)
		}
	}
	if a.RecordModelCall("S1", riskModel, &noWait429) != CallRisky {
		t.Fatal("no risk on the third 429 with nobody else calling")
	}
}

func TestNoRiskWhenPeersAreRefusedToo(t *testing.T) {
	a := riskAccount(t)
	a.RecordModelCall("S1", riskModel, &noWait429)
	a.RecordModelCall("S2", riskModel, nil)
	a.RecordModelCall("S3", riskModel, &noWait429)
	for i := 0; i < 5; i++ {
		if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
			t.Fatal("risk although another session was refused the same way")
		}
	}
}

func TestPeersOnOtherModelsDoNotCount(t *testing.T) {
	a := riskAccount(t)
	a.RecordModelCall("S1", riskModel, &noWait429)
	a.RecordModelCall("S2", "claude-sonnet-4-6", nil)
	if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
		t.Fatal("a call on another model made the second 429 risk")
	}
	if a.RecordModelCall("S1", riskModel, &noWait429) != CallRisky {
		t.Fatal("no risk on the third 429")
	}
}

func TestPeersBeforeTheStreakDoNotCount(t *testing.T) {
	a := riskAccount(t)
	a.RecordModelCall("S2", riskModel, nil)
	time.Sleep(2 * time.Millisecond)
	a.RecordModelCall("S1", riskModel, &noWait429)
	if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
		t.Fatal("a peer served before the streak made the second 429 risk")
	}
}

func TestOtherOutcomesBreakTheStreak(t *testing.T) {
	for name, outcome := range map[string]*ModelFailure{"success": nil, "429 with a wait": &quota429, "500": {Code: 500}} {
		a := riskAccount(t)
		a.RecordModelCall("S1", riskModel, &noWait429)
		a.RecordModelCall("S1", riskModel, outcome)
		for i := 1; i <= 2; i++ {
			if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
				t.Errorf("%s: risk on 429 %d of a new streak", name, i)
			}
		}
		if a.RecordModelCall("S1", riskModel, &noWait429) != CallRisky {
			t.Errorf("%s: no risk on the third 429 of a new streak", name)
		}
	}
}

func TestForgetModelFailureEndsTheStreak(t *testing.T) {
	a := riskAccount(t)
	a.RecordModelCall("S1", riskModel, &noWait429)
	a.RecordModelCall("S1", riskModel, &noWait429)
	a.ForgetModelFailure("S1")
	if a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky || a.RecordModelCall("S1", riskModel, &noWait429) == CallRisky {
		t.Fatal("the streak survived the turn")
	}
}

func TestOldCallsAreForgotten(t *testing.T) {
	a := riskAccount(t)
	a.RecordModelCall("S2", riskModel, nil)
	a.mu.Lock()
	a.calls[0].at = time.Now().Add(-callHistory - time.Second)
	a.mu.Unlock()
	a.RecordModelCall("S1", riskModel, nil)
	a.mu.Lock()
	kept := len(a.calls)
	a.mu.Unlock()
	if kept != 1 {
		t.Fatalf("kept %d calls, want only the recent one", kept)
	}
}

var serverError = ModelFailure{Code: http.StatusBadGateway, Message: "empty response"}

func TestSessionIsStuckAfterConsecutiveFailedCalls(t *testing.T) {
	a := riskAccount(t)
	for i := 1; i < stuckCallStreak; i++ {
		if got := a.RecordModelCall("S1", riskModel, &serverError); got != CallOK {
			t.Fatalf("verdict %v on failed call %d", got, i)
		}
	}
	if got := a.RecordModelCall("S1", riskModel, &serverError); got != CallStuck {
		t.Fatalf("verdict %v on failed call %d, want CallStuck", got, stuckCallStreak)
	}
	if got := a.RecordModelCall("S1", riskModel, &serverError); got != CallOK {
		t.Fatalf("stuck reported twice for one turn: %v", got)
	}
}

func TestSuccessOrForgettingEndsTheFailedCallRun(t *testing.T) {
	ends := map[string]func(*Account){
		"success": func(a *Account) { a.RecordModelCall("S1", riskModel, nil) },
		"forget":  func(a *Account) { a.ForgetModelFailure("S1") },
	}
	for name, end := range ends {
		a := riskAccount(t)
		for i := 1; i < stuckCallStreak; i++ {
			a.RecordModelCall("S1", riskModel, &serverError)
		}
		end(a)
		for i := 1; i < stuckCallStreak; i++ {
			if got := a.RecordModelCall("S1", riskModel, &serverError); got != CallOK {
				t.Fatalf("%s: verdict %v on call %d of a new run", name, got, i)
			}
		}
	}
}

func TestFailedCallRunsAreCountedPerSession(t *testing.T) {
	a := riskAccount(t)
	for i := 1; i < stuckCallStreak; i++ {
		a.RecordModelCall("S1", riskModel, &serverError)
		if got := a.RecordModelCall("S2", riskModel, &serverError); got != CallOK {
			t.Fatalf("S2 verdict %v on call %d", got, i)
		}
	}
}

func TestTruncatedCallAnswersAndIsRemembered(t *testing.T) {
	a := riskAccount(t)
	for i := 1; i < stuckCallStreak; i++ {
		a.RecordModelCall("S1", riskModel, &serverError)
	}
	a.RecordModelCall("S1", riskModel, &ModelFailure{Code: http.StatusOK, Truncated: true})
	if !a.Truncated("S1") || a.ModelFailure("S1") != nil {
		t.Fatalf("truncated = %v, failure = %+v", a.Truncated("S1"), a.ModelFailure("S1"))
	}
	for i := 1; i < stuckCallStreak; i++ {
		if got := a.RecordModelCall("S1", riskModel, &serverError); got != CallOK {
			t.Fatalf("verdict %v on call %d after a truncated one", got, i)
		}
	}
	if a.Truncated("S1") || a.ModelFailure("S1") == nil {
		t.Fatal("a later failed call did not replace the truncated one")
	}

	a.RecordModelCall("S1", riskModel, &ModelFailure{Code: http.StatusOK, Truncated: true})
	a.RecordModelCall("S1", riskModel, nil)
	if a.Truncated("S1") {
		t.Fatal("a later success did not replace the truncated call")
	}

	a.RecordModelCall("S1", riskModel, &ModelFailure{Code: http.StatusOK, Truncated: true})
	a.ForgetModelFailure("S1")
	if a.Truncated("S1") {
		t.Fatal("forgetting kept the truncated call")
	}
}
