package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
)

// ModelCost matches usage.ModelCost structure
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// UsageReport matches the payload FastAgent sends to webhook
type UsageReport struct {
	UserID     string `json:"userId"`
	AgentID    string `json:"agentId"`
	SessionID  string `json:"sessionId,omitempty"`
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
	Usage      struct {
		InputTokens         int `json:"inputTokens"`
		OutputTokens        int `json:"outputTokens"`
		CacheReadTokens     int `json:"cacheReadTokens,omitempty"`
		CacheCreationTokens int `json:"cacheCreationTokens,omitempty"`
	} `json:"usage"`
	Cost         ModelCost `json:"cost"`
	RawCostCents int       `json:"rawCostCents"`
	Timestamp    string    `json:"timestamp"`
}

var (
	receivedPayloads []UsageReport
	mu               sync.Mutex
)

func main() {
	port := "9999"
	token := os.Getenv("WEBHOOK_TEST_TOKEN")
	if token == "" {
		token = "test-webhook-token-123"
	}

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	// Webhook endpoint (mimics Cloud's /api/fastagent/webhooks/usage)
	mux.HandleFunc("/api/fastagent/webhooks/usage", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// Verify auth
		auth := r.Header.Get("Authorization")
		expectedAuth := "Bearer " + token

		body, _ := io.ReadAll(r.Body)
		log.Printf("=== Webhook Received ===")
		log.Printf("Method: %s", r.Method)
		log.Printf("Content-Type: %s", r.Header.Get("Content-Type"))
		log.Printf("Authorization: %s", auth)
		log.Printf("Body: %s", string(body))
		log.Printf("=========================")

		if auth != expectedAuth {
			log.Printf("Auth mismatch: got %q, want %q", auth, expectedAuth)
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"invalid token"}`)
			return
		}

		var report UsageReport
		if err := json.Unmarshal(body, &report); err != nil {
			log.Printf("JSON parse error: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"invalid json"}`)
			return
		}

		receivedPayloads = append(receivedPayloads, report)

		// Always return 200 (per design: never let retry cause double-charge)
		w.WriteHeader(http.StatusOK)
		response := fmt.Sprintf(`{"ok":true,"received":{"userId":"%s","modelId":"%s","rawCostCents":%d}}`,
			report.UserID, report.ModelID, report.RawCostCents)
		fmt.Fprint(w, response)
	})

	// Stats endpoint to view received webhooks
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		totalInput := 0
		totalOutput := 0
		totalCost := 0
		byUser := make(map[string]int)

		for _, p := range receivedPayloads {
			totalInput += p.Usage.InputTokens
			totalOutput += p.Usage.OutputTokens
			totalCost += p.RawCostCents
			byUser[p.UserID]++
		}

		stats := map[string]interface{}{
			"totalWebhooks":     len(receivedPayloads),
			"totalInputTokens":  totalInput,
			"totalOutputTokens": totalOutput,
			"totalCostCents":    totalCost,
			"byUser":            byUser,
			"payloads":          receivedPayloads,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	// Reset stats
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPayloads = nil
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true,"msg":"stats reset"}`)
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		<-sigChan
		log.Println("Shutting down...")
		server.Close()
	}()

	log.Printf("=== Webhook Test Server ===")
	log.Printf("Listening on http://localhost:%s", port)
	log.Printf("Webhook URL: http://localhost:%s/api/fastagent/webhooks/usage", port)
	log.Printf("Webhook Token: %s", token)
	log.Printf("===========================")
	log.Printf("Endpoints:")
	log.Printf("  POST /api/fastagent/webhooks/usage  - webhook receiver")
	log.Printf("  GET  /health                       - health check")
	log.Printf("  GET  /stats                        - view received webhooks")
	log.Printf("  GET  /reset                        - reset stats")

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	// Print summary on exit
	log.Printf("\n=== Session Summary ===")
	log.Printf("Total webhooks received: %d", len(receivedPayloads))
	log.Printf("Press Ctrl+C again to exit")

	// Wait for Ctrl+C again
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	select {}
}
