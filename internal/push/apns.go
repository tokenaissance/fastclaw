package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type APNSClient struct {
	teamID     string
	keyID      string
	bundleID   string
	privateKey *ecdsa.PrivateKey
	endpoint   string
	httpClient *http.Client

	mu        sync.Mutex
	jwt       string
	jwtExpiry time.Time
}

type Notification struct {
	DeviceToken string
	Title       string
	Body        string
	AgentID     string
	SessionID   string
}

func NewAPNSClientFromEnv() *APNSClient {
	teamID := strings.TrimSpace(os.Getenv("APNS_TEAM_ID"))
	keyID := strings.TrimSpace(os.Getenv("APNS_KEY_ID"))
	bundleID := strings.TrimSpace(os.Getenv("APNS_BUNDLE_ID"))
	keyPEM := strings.TrimSpace(os.Getenv("APNS_PRIVATE_KEY"))
	if keyPEM == "" {
		if path := strings.TrimSpace(os.Getenv("APNS_PRIVATE_KEY_FILE")); path != "" {
			if data, err := os.ReadFile(path); err == nil {
				keyPEM = string(data)
			} else {
				slog.Warn("read APNS_PRIVATE_KEY_FILE failed", "error", err)
			}
		}
	}
	if teamID == "" || keyID == "" || bundleID == "" || keyPEM == "" {
		return nil
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		slog.Warn("APNs disabled: invalid private key", "error", err)
		return nil
	}
	endpoint := "https://api.push.apple.com"
	if strings.EqualFold(os.Getenv("APNS_ENV"), "sandbox") || strings.EqualFold(os.Getenv("APNS_ENVIRONMENT"), "sandbox") {
		endpoint = "https://api.sandbox.push.apple.com"
	}
	return &APNSClient{
		teamID:     teamID,
		keyID:      keyID,
		bundleID:   bundleID,
		privateKey: key,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *APNSClient) Enabled() bool {
	return c != nil && c.privateKey != nil
}

func (c *APNSClient) Send(ctx context.Context, n Notification) error {
	if !c.Enabled() {
		return nil
	}
	token := strings.TrimSpace(n.DeviceToken)
	if token == "" {
		return errors.New("apns: empty device token")
	}
	body := strings.TrimSpace(n.Body)
	if body == "" {
		body = "你有一条新的 Agent 消息"
	}
	title := strings.TrimSpace(n.Title)
	if title == "" {
		title = "FastAgent"
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": title,
				"body":  body,
			},
			"sound": "default",
		},
		"agentId":   n.AgentID,
		"sessionId": n.SessionID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/3/device/"+token, bytes.NewReader(data))
	if err != nil {
		return err
	}
	jwt, err := c.authJWT()
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", c.bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("apns: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func (c *APNSClient) authJWT() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if c.jwt != "" && now.Before(c.jwtExpiry) {
		return c.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": c.teamID, "iat": now.Unix()})
	signingInput := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := ecdsa.SignASN1(rand.Reader, c.privateKey, sum[:])
	if err != nil {
		return "", err
	}
	c.jwt = signingInput + "." + b64(sig)
	c.jwtExpiry = now.Add(45 * time.Minute)
	return c.jwt, nil
}

func b64(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func parsePrivateKey(raw string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, errors.New("missing PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not ECDSA")
	}
	return ecdsaKey, nil
}
