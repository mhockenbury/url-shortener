package shortener

import (
	"math"
	"testing"
)

func TestEncodeKnownValues(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "A"},
		{35, "Z"},
		{36, "a"},
		{61, "z"},
		{62, "10"},
		{63, "11"},
		{62*62 - 1, "zz"},
		{62 * 62, "100"},
	}
	for _, c := range cases {
		got := Encode(c.in)
		if got != c.want {
			t.Errorf("Encode(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodeKnownValues(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"1", 1},
		{"9", 9},
		{"A", 10},
		{"Z", 35},
		{"a", 36},
		{"z", 61},
		{"10", 62},
		{"11", 63},
		{"zz", 62*62 - 1},
		{"100", 62 * 62},
	}
	for _, c := range cases {
		got, err := Decode(c.in)
		if err != nil {
			t.Errorf("Decode(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Decode(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 2, 61, 62, 100, 1000, 1_000_000,
		62 * 62 * 62, // "1000"
		math.MaxUint32,
		math.MaxUint64,
	}
	for _, v := range values {
		s := Encode(v)
		back, err := Decode(s)
		if err != nil {
			t.Errorf("round trip Decode(%q) from Encode(%d) failed: %v", s, v, err)
			continue
		}
		if back != v {
			t.Errorf("round trip: Encode(%d) = %q, Decode(%q) = %d", v, s, s, back)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []string{
		"",
		"!",
		"abc$",
		"12 3",
		"中",
	}
	for _, s := range cases {
		if _, err := Decode(s); err != ErrInvalidCode {
			t.Errorf("Decode(%q) err = %v, want ErrInvalidCode", s, err)
		}
	}
}

func TestIsValidCode(t *testing.T) {
	valid := []string{"0", "a", "Z", "aB3cD4e", "zzzzzzzzzzz"}
	invalid := []string{"", "!", "foo-bar", "hello world", "中"}
	for _, s := range valid {
		if !IsValidCode(s) {
			t.Errorf("IsValidCode(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if IsValidCode(s) {
			t.Errorf("IsValidCode(%q) = true, want false", s)
		}
	}
}

// Encoded length for max uint64 should fit the stack buffer declared in Encode.
func TestEncodeMaxUint64Length(t *testing.T) {
	s := Encode(math.MaxUint64)
	if len(s) == 0 || len(s) > 11 {
		t.Errorf("Encode(MaxUint64) length = %d, want 1..11", len(s))
	}
	back, err := Decode(s)
	if err != nil || back != math.MaxUint64 {
		t.Errorf("round trip MaxUint64: got (%d, %v)", back, err)
	}
}

// Property: Encode never produces leading zeros for values > 0.
// "0" is the only valid zero-prefix output (the literal encoding of 0).
func TestEncodeNoLeadingZeroForNonZero(t *testing.T) {
	for _, v := range []uint64{1, 61, 62, 63, 123456, math.MaxUint64} {
		s := Encode(v)
		if len(s) > 1 && s[0] == '0' {
			t.Errorf("Encode(%d) = %q has leading zero", v, s)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Encode(uint64(i))
	}
}

func BenchmarkDecode(b *testing.B) {
	s := Encode(1_234_567_890)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decode(s)
	}
}
