// Package shortener contains the short-code generator and the link
// registration flow (validate -> Safe Browsing -> conditional insert).
package shortener

import (
	"crypto/rand"
	"errors"
	"io"
)

// Alphabet is the Base62 alphabet used for short codes.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CodeLength is the fixed length of every short code (bit.ly style, 62^7 ≈ 3.5e12).
const CodeLength = 7

// rejectionLimit is the largest multiple of 62 that fits in a byte (62*4).
// Bytes >= this value are discarded so that every symbol is equally likely
// (no modulo bias).
const rejectionLimit = 248

// NewCode returns a uniformly random 7-character Base62 code read from r.
// If r is nil, crypto/rand.Reader is used.
func NewCode(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	out := make([]byte, 0, CodeLength)
	buf := make([]byte, 16)
	for len(out) < CodeLength {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", errors.Join(errors.New("shortener: random source failed"), err)
		}
		for _, b := range buf {
			if b >= rejectionLimit {
				continue
			}
			out = append(out, Alphabet[b%62])
			if len(out) == CodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// IsValidCode reports whether s has the exact shape of a short code.
func IsValidCode(s string) bool {
	if len(s) != CodeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}
