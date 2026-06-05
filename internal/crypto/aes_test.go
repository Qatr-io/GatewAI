package crypto_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"gatewai/gateway/internal/crypto"
)

// ── ParseKey ─────────────────────────────────────────────────────────────────

func TestParseKey_Empty(t *testing.T) {
	key, err := crypto.ParseKey("")
	if key != nil || err != nil {
		t.Errorf("empty key: expected (nil, nil), got (%v, %v)", key, err)
	}
}

func TestParseKey_Valid(t *testing.T) {
	key, err := crypto.ParseKey(strings.Repeat("ab", 32)) // 64 hex chars = 32 bytes
	if err != nil {
		t.Fatalf("valid key: unexpected error: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("valid key: expected 32 bytes, got %d", len(key))
	}
}

func TestParseKey_InvalidHex(t *testing.T) {
	_, err := crypto.ParseKey("not-valid-hex!!")
	if err == nil {
		t.Error("invalid hex: expected error, got nil")
	}
}

func TestParseKey_WrongLength(t *testing.T) {
	// 16 bytes (AES-128) instead of 32 (AES-256)
	_, err := crypto.ParseKey(strings.Repeat("ab", 16))
	if err == nil {
		t.Error("wrong length: expected error, got nil")
	}
}

// ── EncryptedSize ─────────────────────────────────────────────────────────────

func TestEncryptedSize_Negative(t *testing.T) {
	if got := crypto.EncryptedSize(-1); got != -1 {
		t.Errorf("expected -1 for negative input, got %d", got)
	}
}

func TestEncryptedSize_MatchesActual(t *testing.T) {
	key, _ := crypto.ParseKey(strings.Repeat("cd", 32))

	sizes := []int64{0, 1, 64*1024 - 1, 64 * 1024, 64*1024 + 1, 128 * 1024, 128*1024 + 7}
	for _, plainSize := range sizes {
		t.Run(fmt.Sprintf("plain_%d", plainSize), func(t *testing.T) {
			data := make([]byte, plainSize)
			enc := crypto.Encrypt(key, bytes.NewReader(data))
			ciphertext, err := io.ReadAll(enc)
			enc.Close()
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			expected := crypto.EncryptedSize(plainSize)
			if int64(len(ciphertext)) != expected {
				t.Errorf("EncryptedSize(%d) = %d, actual = %d", plainSize, expected, len(ciphertext))
			}
		})
	}
}

// ── Encrypt / Decrypt round-trip ──────────────────────────────────────────────

func roundTrip(t *testing.T, key []byte, plaintext []byte) []byte {
	t.Helper()
	enc := crypto.Encrypt(key, bytes.NewReader(plaintext))
	dec := crypto.Decrypt(key, enc)
	result, err := io.ReadAll(dec)
	dec.Close()
	if err != nil {
		t.Fatalf("round-trip error: %v", err)
	}
	return result
}

func TestRoundTrip(t *testing.T) {
	key, _ := crypto.ParseKey(strings.Repeat("ef", 32))

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"single_byte", []byte{0x42}},
		{"small", []byte("hello, AES-GCM world")},
		{"just_under_chunk", make([]byte, 64*1024-1)},
		{"exact_chunk", make([]byte, 64*1024)},
		{"chunk_plus_one", make([]byte, 64*1024+1)},
		{"two_chunks", make([]byte, 128*1024)},
		{"two_chunks_plus_seven", make([]byte, 128*1024+7)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Fill with non-zero pattern to catch offset bugs.
			for i := range tc.data {
				tc.data[i] = byte(i)
			}
			got := roundTrip(t, key, tc.data)
			if !bytes.Equal(got, tc.data) {
				t.Errorf("round-trip mismatch: want %d bytes, got %d bytes", len(tc.data), len(got))
			}
		})
	}
}

// ── Nil key passthrough ───────────────────────────────────────────────────────

func TestEncrypt_NilKey_Passthrough(t *testing.T) {
	data := []byte("plaintext passthrough")
	rc := crypto.Encrypt(nil, bytes.NewReader(data))
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("nil key Encrypt should return data unchanged")
	}
}

func TestDecrypt_NilKey_Passthrough(t *testing.T) {
	data := []byte("plaintext passthrough")
	rc := crypto.Decrypt(nil, io.NopCloser(bytes.NewReader(data)))
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("nil key Decrypt should return data unchanged")
	}
}

// ── Authentication tag verification ──────────────────────────────────────────

func TestDecrypt_TamperedCiphertext_ReturnsError(t *testing.T) {
	key, _ := crypto.ParseKey(strings.Repeat("12", 32))
	plaintext := []byte("sensitive payload that must not be forged")

	enc := crypto.Encrypt(key, bytes.NewReader(plaintext))
	ciphertext, err := io.ReadAll(enc)
	enc.Close()
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	// Flip a byte in the ciphertext body (after the 8-byte nonce prefix).
	if len(ciphertext) > 20 {
		ciphertext[15] ^= 0xFF
	}

	dec := crypto.Decrypt(key, io.NopCloser(bytes.NewReader(ciphertext)))
	_, err = io.ReadAll(dec)
	dec.Close()
	if err == nil {
		t.Error("expected authentication error for tampered ciphertext, got nil")
	}
}

func TestDecrypt_WrongKey_ReturnsError(t *testing.T) {
	key1, _ := crypto.ParseKey(strings.Repeat("aa", 32))
	key2, _ := crypto.ParseKey(strings.Repeat("bb", 32))

	enc := crypto.Encrypt(key1, bytes.NewReader([]byte("secret")))
	ciphertext, _ := io.ReadAll(enc)
	enc.Close()

	dec := crypto.Decrypt(key2, io.NopCloser(bytes.NewReader(ciphertext)))
	_, err := io.ReadAll(dec)
	dec.Close()
	if err == nil {
		t.Error("expected error when decrypting with wrong key, got nil")
	}
}

func TestDecrypt_TruncatedStream_ReturnsError(t *testing.T) {
	key, _ := crypto.ParseKey(strings.Repeat("34", 32))
	plaintext := make([]byte, 1024)

	enc := crypto.Encrypt(key, bytes.NewReader(plaintext))
	ciphertext, _ := io.ReadAll(enc)
	enc.Close()

	// Keep only half the ciphertext.
	truncated := ciphertext[:len(ciphertext)/2]

	dec := crypto.Decrypt(key, io.NopCloser(bytes.NewReader(truncated)))
	_, err := io.ReadAll(dec)
	dec.Close()
	if err == nil {
		t.Error("expected error for truncated ciphertext, got nil")
	}
}
