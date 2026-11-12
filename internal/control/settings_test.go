package control

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

func TestLoadSettingsPrices(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Saved before cache_write existed, with a custom model at the index of
	// the claude-opus-5-5 default, plus one row that saved a 0 explicitly.
	stored := map[string]any{"prices": []map[string]any{
		{"model": "gemini-4-argon", "input": 2, "cached": 0.1, "output": 10},
		{"model": "claude-sonnet-5-5", "input": 2, "cached": 0.1, "output": 10},
		{"model": "test-model", "input": 1, "cached": 0.1, "output": 2},
		{"model": "claude-opus-5-5", "input": 4, "cached": 0.2, "cache_write": 0, "output": 20},
	}}
	if err := st.SetSetting(context.Background(), settingsKey, stored); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, p := range s.Prices {
		got[p.Model] = p.CacheWrite
	}
	want := map[string]float64{"gemini-4-argon": 0, "claude-sonnet-5-5": 2.5, "test-model": 0, "claude-opus-5-5": 0}
	if len(s.Prices) != len(want) {
		t.Fatalf("prices = %+v, want only the stored rows", s.Prices)
	}
	for model, w := range want {
		if got[model] != w {
			t.Errorf("%s cache_write = %v, want %v", model, got[model], w)
		}
	}
	if len(s.ContextWindows) != len(gateway.DefaultContextWindows) || s.ContextCheck {
		t.Fatalf("context check %v with windows %+v, want it off with the default windows when none are stored", s.ContextCheck, s.ContextWindows)
	}
}

func TestLoadSettingsContextWindows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Stored windows replace the defaults whole, including an empty list.
	for _, stored := range [][]gateway.ContextWindow{{{Model: "test-model", Tokens: 5000}}, {}} {
		if err := st.SetSetting(context.Background(), settingsKey, map[string]any{"context_windows": stored}); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings(context.Background(), st)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.ContextWindows, stored) {
			t.Fatalf("context windows = %+v, want %+v", s.ContextWindows, stored)
		}
	}

	bad := DefaultSettings()
	bad.ContextWindows = []gateway.ContextWindow{{Model: "m", Tokens: 0}}
	if err := bad.validate(); err == nil {
		t.Fatal("a zero window passed")
	}
}
