package main

import (
	"context"
	"testing"
	"time"
)

// Regression: CollectAll must return the connection error (the GUI crashed when a
// swallowed error led to an empty result being shown).
func TestCollectAllReportsErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inv, sheets, _, _, err := CollectAll(ctx, "127.0.0.1:1", "u", "p", true)
	if err == nil {
		t.Fatalf("expected an error, got inv=%v sheets=%d", inv != nil, len(sheets))
	}
}
