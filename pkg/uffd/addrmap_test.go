package uffd

import "testing"

func TestLocateForDisambiguatesEqualVirtualAddresses(t *testing.T) {
	m := NewAddressMap(4 * PageSize)
	const va = uint64(0x100000)
	if err := m.RegisterVMA(ProcessBackend, va, 2*PageSize, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterVMA(ProcessCH, va, 2*PageSize, 2*PageSize); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.LocateFor(ProcessCH, va+PageSize); !ok || got != 3*PageSize {
		t.Fatalf("CH got=%x ok=%v", got, ok)
	}
	if got, ok := m.LocateFor(ProcessBackend, va+PageSize); !ok || got != PageSize {
		t.Fatalf("backend got=%x ok=%v", got, ok)
	}
}
