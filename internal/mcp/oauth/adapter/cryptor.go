package adapter

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

// AESGCMCryptor encrypts credentials with AES-256-GCM using a key derived
// from the configured secret. Fails closed: no secret, no encryption.
type AESGCMCryptor struct {
	key []byte
}

// NewAESGCMCryptor derives the 32-byte key from secret via SHA-256.
func NewAESGCMCryptor(secret string) (*AESGCMCryptor, error) {
	if secret == "" {
		return nil, errors.New("oauth: encryption secret is required (FASTAGENT_OAUTH_SECRET)")
	}
	sum := sha256.Sum256([]byte(secret))
	return &AESGCMCryptor{key: sum[:]}, nil
}

// Encrypt seals plaintext with a random nonce.
func (c *AESGCMCryptor) Encrypt(_ context.Context, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt opens a sealed ciphertext.
func (c *AESGCMCryptor) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("oauth: ciphertext too short")
	}
	nonce, data := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, data, nil)
}
