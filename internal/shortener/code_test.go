package shortener

import (
	"bytes"
	"errors"
	"testing"
)

func TestNewCodeShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20000; i++ {
		c, err := NewCode(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !IsValidCode(c) {
			t.Fatalf("invalid code %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code after %d iterations: %q", i, c)
		}
		seen[c] = true
	}
}

func TestNewCodeRejectsBiasedBytes(t *testing.T) {
	// 248..255 must be skipped; 0 -> '0', 61 -> 'z', 62 -> '0' (62 % 62).
	src := append(bytes.Repeat([]byte{255}, 16), []byte{0, 61, 62, 247, 248, 1, 2, 3, 0, 0, 0, 0, 0, 0, 0, 0}...)
	c, err := NewCode(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := "0z0z123"; c != want { // 247 % 62 = 61 -> 'z'
		t.Fatalf("got %q want %q", c, want)
	}
}

func TestNewCodeDistribution(t *testing.T) {
	counts := map[byte]int{}
	const n = 62 * 2000
	for i := 0; i < n/CodeLength; i++ {
		c, _ := NewCode(nil)
		for j := 0; j < len(c); j++ {
			counts[c[j]]++
		}
	}
	if len(counts) != 62 {
		t.Fatalf("expected all 62 symbols, got %d", len(counts))
	}
	for s, k := range counts {
		if k < 1400 || k > 2600 { // expected ~2000 each
			t.Errorf("symbol %q count %d looks biased", s, k)
		}
	}
}

func TestNewCodeRandomFailure(t *testing.T) {
	if _, err := NewCode(bytes.NewReader(nil)); err == nil {
		t.Fatal("expected error from empty random source")
	}
	_, err := NewCode(errReader{})
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

var errBoom = errors.New("boom")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

func TestIsValidCode(t *testing.T) {
	for _, c := range []string{"abcDEF1", "0000000", "zzzzzzz"} {
		if !IsValidCode(c) {
			t.Errorf("%q should be valid", c)
		}
	}
	for _, c := range []string{"", "abc", "abcdefgh", "abc-def", "abc_def", "ab/cdef", "ａbcdefg", "healthz"[:6]} {
		if IsValidCode(c) {
			t.Errorf("%q should be invalid", c)
		}
	}
}
