package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestParseTriageDecision(t *testing.T) {
	ok := `{"category":"notify","urgency":"high","confidence":0.7,"summary":"s"}`
	cases := []struct {
		name    string
		in      string
		wantErr bool
		check   func(t *testing.T, d TriageDecision)
	}{
		{name: "plain", in: ok, check: func(t *testing.T, d TriageDecision) {
			if d.Category != CategoryNotify || d.Urgency != UrgencyHigh || d.Confidence != 0.7 || d.Summary != "s" {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "fenced", in: "```json\n" + ok + "\n```", check: func(t *testing.T, d TriageDecision) {
			if d.Category != CategoryNotify {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "prose prefix", in: "Here is my answer: " + ok + " thanks", check: func(t *testing.T, d TriageDecision) {
			if d.Category != CategoryNotify {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "two objects first wins", in: `{"category":"ignore","confidence":0.9,"summary":"a"} {"category":"notify","confidence":0.1,"summary":"b"}`, check: func(t *testing.T, d TriageDecision) {
			if d.Category != CategoryIgnore || d.Summary != "a" {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "braces in string", in: `{"category":"notify","confidence":0.5,"summary":"a } b {"}`, check: func(t *testing.T, d TriageDecision) {
			if d.Summary != "a } b {" {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "confidence high", in: `{"category":"notify","confidence":1.5,"summary":"s"}`, wantErr: true},
		{name: "confidence negative", in: `{"category":"notify","confidence":-0.1,"summary":"s"}`, wantErr: true},
		{name: "unknown category", in: `{"category":"panic","confidence":0.5,"summary":"s"}`, wantErr: true},
		{name: "unknown urgency", in: `{"category":"notify","urgency":"asap","confidence":0.5,"summary":"s"}`, wantErr: true},
		{name: "missing urgency defaults normal", in: `{"category":"notify","confidence":0.5,"summary":"s"}`, check: func(t *testing.T, d TriageDecision) {
			if d.Urgency != UrgencyNormal {
				t.Fatalf("got %q", d.Urgency)
			}
		}},
		{name: "investigate without goal", in: `{"category":"investigate","confidence":0.9,"summary":"s"}`, wantErr: true},
		{name: "investigate with goal", in: `{"category":"investigate","confidence":0.9,"summary":"s","goal":"g"}`, check: func(t *testing.T, d TriageDecision) {
			if d.Goal != "g" {
				t.Fatalf("got %+v", d)
			}
		}},
		{name: "summary too long", in: `{"category":"notify","confidence":0.5,"summary":"` + strings.Repeat("ữ", 201) + `"}`, wantErr: true},
		{name: "summary 200 runes ok", in: `{"category":"notify","confidence":0.5,"summary":"` + strings.Repeat("ữ", 200) + `"}`},
		{name: "goal too long", in: `{"category":"investigate","confidence":0.5,"summary":"s","goal":"` + strings.Repeat("g", 301) + `"}`, wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "no json", in: "I cannot help", wantErr: true},
		{name: "unterminated", in: `{"category":"notify"`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseTriageDecision(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", d)
				}
				if !errors.Is(err, ErrInvalidDecision) {
					t.Fatalf("error %v does not wrap ErrInvalidDecision", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

func TestRoute(t *testing.T) {
	p := RoutingPolicy{Threshold: 0.85, ToolsAvailable: true}
	noTools := RoutingPolicy{Threshold: 0.85}
	cases := []struct {
		name string
		d    TriageDecision
		p    RoutingPolicy
		want Outcome
	}{
		{"ignore confident", TriageDecision{Category: CategoryIgnore, Confidence: 0.9}, p, OutcomeIgnore},
		{"ignore unsure", TriageDecision{Category: CategoryIgnore, Confidence: 0.5}, p, OutcomeNotify},
		{"ignore at threshold", TriageDecision{Category: CategoryIgnore, Confidence: 0.85}, p, OutcomeIgnore},
		{"investigate with tools", TriageDecision{Category: CategoryInvestigate, Confidence: 0.2, Goal: "g"}, p, OutcomeInvestigate},
		{"investigate without tools", TriageDecision{Category: CategoryInvestigate, Confidence: 0.9, Goal: "g"}, noTools, OutcomeNotify},
		{"notify", TriageDecision{Category: CategoryNotify, Confidence: 0.99}, p, OutcomeNotify},
	}
	for _, tc := range cases {
		if got := Route(tc.d, tc.p); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
