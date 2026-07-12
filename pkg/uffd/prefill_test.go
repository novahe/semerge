package uffd

import (
	"context"
	"errors"
	"testing"
)

func TestPrefillCopyHonorsCanceledContextBeforeCopy(t *testing.T) {
	h := &Handler{state: NewPageStateMap(1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	filled, existing, err := h.PrefillCopy(ctx, 0, make([]byte, PageSize))
	if !errors.Is(err, context.Canceled) || filled != 0 || existing != 0 {
		t.Fatalf("filled=%d existing=%d err=%v", filled, existing, err)
	}
}
