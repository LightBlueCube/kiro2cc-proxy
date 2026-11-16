package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestUpstreamCallsFollowTheirRequest(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	if err := st.AddRequest(ctx, Request{At: now, Status: StatusOK, CallToken: "tok-a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRequest(ctx, Request{At: now, Status: StatusOK, CallToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRequest(ctx, Request{At: now, Status: StatusOK}); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("abc "), 50_000)
	for _, c := range []UpstreamCall{
		{At: now, CallToken: "tok-a", Account: "main", Session: "S1", Seq: 1, Model: "m", Status: 200, Request: big, Response: []byte("one"), ResponseTruncated: true},
		{At: now, CallToken: "tok-a", Account: "main", Session: "S1", Seq: 2, Model: "m", Status: 200, Request: []byte("{}")},
		{At: now.Add(-UpstreamRetention - time.Minute), CallToken: "tok-a", Session: "S0", Request: []byte("{}")},
		{At: now, CallToken: "", Session: "none", Request: []byte("{}")},
	} {
		if err := st.AddUpstreamCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.CallToken] = r.Upstream
	}
	if counts["tok-a"] != 2 || counts["tok-b"] != 0 || counts[""] != 0 {
		t.Fatalf("upstream counts = %v, want tok-a 2 (the expired call and the untied one do not count)", counts)
	}

	var idA int64
	for _, r := range rows {
		if r.CallToken == "tok-a" {
			idA = r.ID
		}
	}
	infos, err := st.UpstreamCalls(ctx, idA)
	if err != nil || len(infos) != 2 || infos[0].Seq != 1 || infos[1].Seq != 2 {
		t.Fatalf("UpstreamCalls = %+v, %v", infos, err)
	}
	if infos[0].RequestBytes != int64(len(big)) || infos[0].ResponseBytes != 3 || !infos[0].ResponseTruncated {
		t.Fatalf("sizes = %+v", infos[0])
	}

	call, err := st.UpstreamCall(ctx, infos[0].ID)
	if err != nil || !bytes.Equal(call.Request, big) || string(call.Response) != "one" || call.Session != "S1" || call.CallToken != "tok-a" {
		t.Fatalf("UpstreamCall = %+v, %v", call, err)
	}
	if _, err := st.UpstreamCall(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing call: %v", err)
	}
}

func TestUpstreamCallsExpireAndDelete(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	old := UpstreamCall{At: time.Now().Add(-UpstreamRetention - time.Minute), CallToken: "t", Request: []byte("{}")}
	fresh := UpstreamCall{At: time.Now(), CallToken: "t", Request: []byte("{}")}
	for _, c := range []UpstreamCall{old, fresh} {
		if err := st.AddUpstreamCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpstreamCall(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired call is still served: %v", err)
	}
	if err := st.PruneUpstream(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM upstream_calls`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("%d calls left after prune, %v", left, err)
	}
	if err := st.DeleteUpstreamCalls(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM upstream_calls`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d calls left after delete, %v", left, err)
	}
}
