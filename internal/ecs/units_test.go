package ecs

import "testing"

func TestToBytes(t *testing.T) {
	cases := []struct {
		n    float64
		unit string
		want int64
	}{
		{11, "GB", 11811160064}, // locked example from the spec
		{11, "GiB", 11811160064},
		{11, "KB", 11264},
		{11, "KiB", 11264},
		{1, "MB", 1048576},
		{1, "MiB", 1048576},
		{2, "TB", 2199023255552},
		{512, "B", 512},
		{0, "GB", 0},
		{1.5, "GB", 1610612736},
	}
	for _, c := range cases {
		got, err := ToBytes(c.n, c.unit)
		if err != nil {
			t.Fatalf("ToBytes(%v, %q) error: %v", c.n, c.unit, err)
		}
		if got != c.want {
			t.Errorf("ToBytes(%v, %q) = %d, want %d", c.n, c.unit, got, c.want)
		}
	}
}

func TestToBytesUnknownUnit(t *testing.T) {
	if _, err := ToBytes(1, "furlongs"); err == nil {
		t.Fatal("expected error for unknown unit")
	}
	if _, err := ToBytes(1, "PB"); err == nil {
		t.Fatal("expected error for unsupported unit PB (not in the binary table)")
	}
}
