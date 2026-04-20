package shortener

import "errors"

// Alphabet is the base62 alphabet used for short codes. Order matters:
// Encode/Decode are inverses only with respect to this exact ordering.
// Digits first, then uppercase, then lowercase — matches common base62
// conventions and keeps codes URL-safe without percent-encoding.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const base = uint64(len(Alphabet)) // 62

// ErrInvalidCode is returned by Decode when the input contains a character
// outside the base62 alphabet or is empty.
var ErrInvalidCode = errors.New("invalid base62 code")

// decodeTable maps each byte to its value in the alphabet, or -1 if absent.
// Populated once at package init so Decode is O(len) with no map lookups.
var decodeTable [256]int8

func init() {
	for i := range decodeTable {
		decodeTable[i] = -1
	}
	for i, c := range []byte(Alphabet) {
		decodeTable[c] = int8(i)
	}
}

// Encode converts a uint64 ID to its base62 representation.
// Encode(0) returns "0" — the zero value is a valid code.
func Encode(n uint64) string {
	if n == 0 {
		return "0"
	}
	// Max uint64 in base62 fits in 11 chars; small stack buffer avoids alloc.
	var buf [11]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = Alphabet[n%base]
		n /= base
	}
	return string(buf[i:])
}

// Decode parses a base62 string back to its uint64 value.
// Returns ErrInvalidCode if s is empty or contains invalid characters.
// Decode does not check for uint64 overflow — inputs longer than 11
// characters may silently wrap. Callers producing codes via Encode will
// never hit this; external input should be length-validated upstream.
func Decode(s string) (uint64, error) {
	if len(s) == 0 {
		return 0, ErrInvalidCode
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		v := decodeTable[s[i]]
		if v < 0 {
			return 0, ErrInvalidCode
		}
		n = n*base + uint64(v)
	}
	return n, nil
}

// IsValidCode reports whether s consists entirely of base62 characters
// and is non-empty. Useful for cheap HTTP-layer validation before DB lookup.
func IsValidCode(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if decodeTable[s[i]] < 0 {
			return false
		}
	}
	return true
}

// assert the alphabet has the expected size at package load time.
// A typo in Alphabet would silently corrupt every code; this panics early.
func init() {
	if len(Alphabet) != 62 {
		panic("shortener: base62 alphabet must be exactly 62 characters")
	}
	// No duplicate characters.
	seen := make(map[byte]struct{}, 62)
	for i := 0; i < len(Alphabet); i++ {
		if _, dup := seen[Alphabet[i]]; dup {
			panic("shortener: duplicate character in base62 alphabet")
		}
		seen[Alphabet[i]] = struct{}{}
	}
	// Sanity check that decodeTable is consistent with Alphabet.
	for i := 0; i < len(Alphabet); i++ {
		if decodeTable[Alphabet[i]] != int8(i) {
			panic("shortener: decodeTable inconsistent with Alphabet")
		}
	}
}
