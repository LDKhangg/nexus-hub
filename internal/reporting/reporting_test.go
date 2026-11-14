package reporting

import "testing"

func TestDefaultOwnerPrefs(t *testing.T) {
	prefs := DefaultOwnerPrefs()
	if prefs.Style != StyleShort || prefs.ShowReaction || !prefs.ShowAISummary || !prefs.ShowLinks || !prefs.ShowPRTitles || !prefs.ShowTraceHint || prefs.Language != "" {
		t.Fatalf("unexpected owner defaults: %+v", prefs)
	}
}
