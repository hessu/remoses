package civ

import "testing"

func TestEncodeFrequency(t *testing.T) {
	tests := []struct {
		name string
		hz   uint64
		want [5]byte
	}{
		// byte0 = 1 Hz + 10 Hz, byte4 = 100 MHz + 1 GHz.
		{"zero", 0, [5]byte{0x00, 0x00, 0x00, 0x00, 0x00}},
		{"one hz", 1, [5]byte{0x01, 0x00, 0x00, 0x00, 0x00}},
		{"ten hz", 10, [5]byte{0x10, 0x00, 0x00, 0x00, 0x00}},
		{"hundred hz", 100, [5]byte{0x00, 0x01, 0x00, 0x00, 0x00}},
		{"160m", 1_840_000, [5]byte{0x00, 0x00, 0x84, 0x01, 0x00}},
		{"cw watering hole", 14_025_000, [5]byte{0x00, 0x50, 0x02, 0x14, 0x00}},
		{"odd digits", 14_025_678, [5]byte{0x78, 0x56, 0x02, 0x14, 0x00}},
		{"6m", 50_313_000, [5]byte{0x00, 0x30, 0x31, 0x50, 0x00}},
		{"above 100 MHz", 145_500_000, [5]byte{0x00, 0x00, 0x50, 0x45, 0x01}},
		{"max", maxFrequencyHz, [5]byte{0x99, 0x99, 0x99, 0x99, 0x99}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeFrequency(tc.hz)
			if err != nil {
				t.Fatalf("encodeFrequency(%d): %v", tc.hz, err)
			}
			if got != tc.want {
				t.Errorf("encodeFrequency(%d) = % X, want % X", tc.hz, got, tc.want)
			}
			back, ok := decodeFrequency(got[:])
			if !ok || back != tc.hz {
				t.Errorf("decodeFrequency(% X) = %d, %v, want %d, true", got, back, ok, tc.hz)
			}
		})
	}
}

func TestEncodeFrequencyOverflow(t *testing.T) {
	if _, err := encodeFrequency(maxFrequencyHz + 1); err == nil {
		t.Fatal("encodeFrequency accepted an 11-digit frequency")
	}
}

func TestFrequencyRoundTrip(t *testing.T) {
	// Sweep the whole range at a stride that hits every digit position.
	for hz := uint64(0); hz <= maxFrequencyHz; hz += 1_234_567 {
		b, err := encodeFrequency(hz)
		if err != nil {
			t.Fatalf("encodeFrequency(%d): %v", hz, err)
		}
		got, ok := decodeFrequency(b[:])
		if !ok || got != hz {
			t.Fatalf("round trip of %d gave %d (ok=%v) via % X", hz, got, ok, b)
		}
	}
}

func TestDecodeFrequencyInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"short", []byte{0x00, 0x00, 0x00, 0x00}},
		{"long", []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{"empty", nil},
		{"bad low nibble", []byte{0x0F, 0x00, 0x00, 0x00, 0x00}},
		{"bad high nibble", []byte{0x00, 0x00, 0xA0, 0x00, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if hz, ok := decodeFrequency(tc.in); ok {
				t.Errorf("decodeFrequency(% X) = %d, true; want not ok", tc.in, hz)
			}
		})
	}
}

func TestBCD2(t *testing.T) {
	for v := 0; v <= 9999; v++ {
		b := encodeBCD2(v)
		got, ok := decodeBCD2(b[:])
		if !ok || got != v {
			t.Fatalf("BCD2 round trip of %d gave %d (ok=%v) via % X", v, got, ok, b)
		}
	}
	if b := encodeBCD2(255); b != [2]byte{0x02, 0x55} {
		t.Errorf("encodeBCD2(255) = % X, want 02 55", b)
	}
	for _, bad := range [][]byte{nil, {0x00}, {0x00, 0x00, 0x00}, {0x0A, 0x00}, {0x00, 0xF0}} {
		if v, ok := decodeBCD2(bad); ok {
			t.Errorf("decodeBCD2(% X) = %d, true; want not ok", bad, v)
		}
	}
}

func TestUnbcdByte(t *testing.T) {
	for v := 0; v <= 99; v++ {
		got, ok := unbcdByte(bcdByte(v))
		if !ok || got != v {
			t.Fatalf("unbcdByte(bcdByte(%d)) = %d, %v", v, got, ok)
		}
	}
	for _, bad := range []byte{0x0A, 0xA0, 0xFF, 0x1F} {
		if _, ok := unbcdByte(bad); ok {
			t.Errorf("unbcdByte(%#X) reported valid BCD", bad)
		}
	}
}
