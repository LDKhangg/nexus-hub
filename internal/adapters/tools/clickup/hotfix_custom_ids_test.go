package clickuptools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

// Hotfix H1 (H1-1): TaskReader.ResolveCustomID against a fake ClickUp.

type h1Hit struct{ path, query, auth string }

func newH1Reader(t *testing.T, r *res, h http.HandlerFunc) (*TaskReader, *[]h1Hit) {
	t.Helper()
	var mu sync.Mutex
	var hits []h1Hit
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		hits = append(hits, h1Hit{req.URL.Path, req.URL.RawQuery, req.Header.Get("Authorization")})
		mu.Unlock()
		h(w, req)
	}))
	t.Cleanup(s.Close)
	tr := NewTaskReader(s.Client(), r)
	tr.c.baseURL = s.URL
	return tr, &hits
}

func h1Code(t *testing.T, err error) string {
	t.Helper()
	var te *tools.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("not a ToolError: %v", err)
	}
	return te.Code
}

// TC-H1-10 (H1-1): path, query and credential of the custom ID read.
func TestHotfixH1_ResolveCustomIDRequest(t *testing.T) {
	rs := &res{val: "pk_secret"}
	tr, hits := newH1Reader(t, rs, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"86canon","custom_id":"FC-6583","name":"Private title"}`))
	})
	id, err := tr.ResolveCustomID(context.Background(), "ws1", "clickup", "FC-6583", "9008218621")
	if err != nil || id != "86canon" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	h := (*hits)[0]
	if len(*hits) != 1 || h.path != "/api/v2/task/FC-6583" || h.query != "custom_task_ids=true&team_id=9008218621" || h.auth != "pk_secret" {
		t.Fatalf("hit=%+v", *hits)
	}
	if rs.ws != "ws1" || rs.ref != "clickup" {
		t.Fatalf("credential asked for ws=%q ref=%q", rs.ws, rs.ref)
	}
}

// TC-H1-11 (H1-1): ClickUp failures map to the safe codes; nothing of the
// response body leaks into the error.
func TestHotfixH1_ResolveCustomIDErrors(t *testing.T) {
	for status, want := range map[int]string{401: "unauthorized", 404: "not_found", 429: "rate_limited", 500: "server_error"} {
		tr, _ := newH1Reader(t, &res{val: "tok"}, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"err":"Team not authorized private-name"}`))
		})
		_, err := tr.ResolveCustomID(context.Background(), "ws", "clickup", "FC-1", "123")
		if got := h1Code(t, err); got != want || strings.Contains(err.Error(), "private") {
			t.Errorf("status %d: code=%q err=%v want %q", status, got, err, want)
		}
	}
	// A missing credential never calls ClickUp.
	for name, rs := range map[string]*res{"no secret": {}, "empty ref": {val: "t"}} {
		tr, hits := newH1Reader(t, rs, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":"x1"}`)) })
		ref := "clickup"
		if name == "empty ref" {
			ref = ""
		}
		_, err := tr.ResolveCustomID(context.Background(), "ws", ref, "FC-1", "123")
		if h1Code(t, err) != "missing_credentials" || len(*hits) != 0 {
			t.Errorf("%s: err=%v hits=%d", name, err, len(*hits))
		}
	}
}

// TC-H1-12 (H1-1): a response without a usable canonical id is invalid_response.
func TestHotfixH1_ResolveCustomIDBadResponse(t *testing.T) {
	for name, body := range map[string]string{
		"not json": `<html>`, "no id": `{"name":"x"}`, "empty id": `{"id":""}`,
		"path id": `{"id":"../admin"}`, "query id": `{"id":"a?b=c"}`, "long id": `{"id":"` + strings.Repeat("a", 65) + `"}`,
		"number id": `{"id":5}`, "array": `[]`,
	} {
		tr, _ := newH1Reader(t, &res{val: "tok"}, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) })
		id, err := tr.ResolveCustomID(context.Background(), "ws", "clickup", "FC-1", "123")
		if err == nil || id != "" || h1Code(t, err) != "invalid_response" {
			t.Errorf("%s: id=%q err=%v", name, id, err)
		}
	}
}

// TC-H1-13 (H1-1, abuse): input that could change the request path or query
// is refused before any request or credential read.
func TestHotfixH1_ResolveCustomIDRejectsInjection(t *testing.T) {
	rs := &res{val: "tok"}
	tr, hits := newH1Reader(t, rs, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":"ok1"}`)) })
	cases := [][2]string{
		{"FC-1/../x", "123"}, {"FC-1?x=y", "123"}, {"FC-1#f", "123"}, {"FC 1", "123"}, {"", "123"}, {"../FC-1", "123"},
		{"FC-1%2F..", "123"}, {strings.Repeat("A", 65), "123"},
		{"FC-1", ""}, {"FC-1", "12a"}, {"FC-1", "123&x=1"}, {"FC-1", "123/../x"}, {"FC-1", "-1"},
		{"FC-1", "999999999999999999999"}, // 21 digits
		{"FC-1", "1 2"},
	}
	for _, c := range cases {
		_, err := tr.ResolveCustomID(context.Background(), "ws", "clickup", c[0], c[1])
		if err == nil || h1Code(t, err) != "invalid_input" {
			t.Errorf("accepted id=%q team=%q err=%v", c[0], c[1], err)
		}
	}
	if len(*hits) != 0 || rs.ref != "" {
		t.Fatalf("a request or credential read happened: hits=%v ref=%q", *hits, rs.ref)
	}
	// The largest allowed team (20 digits) still works.
	if _, err := tr.ResolveCustomID(context.Background(), "ws", "clickup", "FC-1", "12345678901234567890"); err != nil {
		t.Fatalf("20-digit team: %v", err)
	}
}
