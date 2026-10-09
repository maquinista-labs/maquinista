package main

import (
	"strings"
	"testing"
	"time"
)

func TestLedgerCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if strings.HasPrefix(c.Use, "ledger") {
			return
		}
	}
	t.Error("expected 'ledger' command to be registered")
}

func TestLedgerRejectsBadGrain(t *testing.T) {
	rootCmd.SetArgs([]string{"ledger", "--by", "galaxy"})
	if err := rootCmd.Execute(); err == nil {
		t.Error("expected --by galaxy to fail")
	}
}

func TestParseLedgerSince(t *testing.T) {
	if got, err := parseLedgerSince(""); err != nil || got != nil {
		t.Errorf("empty --since: got %v, %v", got, err)
	}

	got, err := parseLedgerSince("720h")
	if err != nil || got == nil {
		t.Fatalf("720h: %v, %v", got, err)
	}
	if want := time.Now().Add(-720 * time.Hour); got.Sub(want) > 2*time.Second {
		t.Errorf("720h: got %v, want ~%v", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	got, err = parseLedgerSince("30d")
	if err != nil || got == nil {
		t.Fatalf("30d: %v, %v", got, err)
	}
	if want := time.Now().Add(-30 * 24 * time.Hour); got.Sub(want) > 2*time.Second {
		t.Errorf("30d: got %v, want ~%v", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	got, err = parseLedgerSince("2026-10-01")
	if err != nil || got == nil {
		t.Fatalf("date: %v, %v", got, err)
	}
	if got.Month() != time.October || got.Day() != 1 || got.Year() != 2026 {
		t.Errorf("date: got %v", got)
	}

	if _, err := parseLedgerSince("yesterday-ish"); err == nil {
		t.Error("expected error for unparsable --since")
	}
}
