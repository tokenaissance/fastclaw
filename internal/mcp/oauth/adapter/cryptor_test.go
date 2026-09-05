package adapter

import (
	"bytes"
	"testing"
)

func TestAESGCMCryptorRoundTrip(t *testing.T) {
	c, err := NewAESGCMCryptor("super-secret")
	if err != nil {
		t.Fatalf("NewAESGCMCryptor: %v", err)
	}
	plain := []byte(`{"access_token":"at","refresh_token":"rt"}`)
	enc, err := c.Encrypt(nil, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(enc, plain) {
		t.Fatal("ciphertext must not contain plaintext")
	}
	dec, err := c.Decrypt(nil, enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("round trip mismatch: %s", dec)
	}
}

func TestAESGCMCryptorFailClosedAndTamper(t *testing.T) {
	if _, err := NewAESGCMCryptor(""); err == nil {
		t.Fatal("empty secret must fail closed")
	}
	c, _ := NewAESGCMCryptor("k")
	enc, _ := c.Encrypt(nil, []byte("data"))
	enc[len(enc)-1] ^= 0xff // flip a byte
	if _, err := c.Decrypt(nil, enc); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestAESGCMCryptorKeyDerivation(t *testing.T) {
	c1, _ := NewAESGCMCryptor("same-secret")
	c2, _ := NewAESGCMCryptor("same-secret")
	enc, _ := c1.Encrypt(nil, []byte("x"))
	if _, err := c2.Decrypt(nil, enc); err != nil {
		t.Fatalf("same secret must decrypt: %v", err)
	}
	c3, _ := NewAESGCMCryptor("other-secret")
	if _, err := c3.Decrypt(nil, enc); err == nil {
		t.Fatal("different secret must fail")
	}
}
