package config

import "testing"

func TestDefaultListen(t *testing.T) {
	t.Run("falls back to :9800 when neither PORT nor LISTEN is set", func(t *testing.T) {
		t.Setenv("PORT", "")
		t.Setenv("LISTEN", "")
		if got := defaultListen(); got != ":9800" {
			t.Fatalf("defaultListen() = %q, want :9800", got)
		}
	})

	t.Run("PORT wins and is normalized to :PORT (PaaS convention)", func(t *testing.T) {
		t.Setenv("PORT", "8080")
		t.Setenv("LISTEN", ":9800")
		if got := defaultListen(); got != ":8080" {
			t.Fatalf("defaultListen() = %q, want :8080", got)
		}
	})

	t.Run("PORT may already carry a colon prefix", func(t *testing.T) {
		t.Setenv("PORT", ":9000")
		t.Setenv("LISTEN", ":9800")
		if got := defaultListen(); got != ":9000" {
			t.Fatalf("defaultListen() = %q, want :9000", got)
		}
	})

	t.Run("LISTEN used when PORT is empty", func(t *testing.T) {
		t.Setenv("PORT", "")
		t.Setenv("LISTEN", "0.0.0.0:1234")
		if got := defaultListen(); got != "0.0.0.0:1234" {
			t.Fatalf("defaultListen() = %q, want 0.0.0.0:1234", got)
		}
	})
}
