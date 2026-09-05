package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// recordingChannelResolver implements api.UserResolver plus the two
// channel hot-register hooks the connect handlers fire through, recording
// which path was taken. #45 makes channels-table-first (RegisterChannel)
// win over the legacy configs fallback (RegisterChannelFromConfig), so a
// connect must produce exactly one ChannelRecord and zero ConfigRecords.
type recordingChannelResolver struct {
	records []store.ChannelRecord
	cfgs    []store.ConfigRecord
}

func (r *recordingChannelResolver) UserSpaceFor(string) (*api.UserSpaceView, error) {
	return nil, nil
}
func (r *recordingChannelResolver) LocalAgentManager() *agent.Manager { return nil }
func (r *recordingChannelResolver) IsCloudMode() bool                 { return true }
func (r *recordingChannelResolver) RegisterChannel(rec store.ChannelRecord) error {
	r.records = append(r.records, rec)
	return nil
}
func (r *recordingChannelResolver) RegisterChannelFromConfig(rec store.ConfigRecord) error {
	r.cfgs = append(r.cfgs, rec)
	return nil
}

// TestChannels_HotRegisterCloudPathE2E mirrors the Cloud call path for the
// #45 hot-register fix: a connect (through the real handler, telegram getMe
// stubbed) must hot-register the just-saved bot from the channels table
// (LookupChannel → hotRegisterChannelRecord) — channels-first — and NOT
// fall back to the legacy configs path while the channels row exists.
func TestChannels_HotRegisterCloudPathE2E(t *testing.T) {
	installFakeTelegram(t, nil)
	s, uid, aid := setupFileUploadTest(t)
	resolver := &recordingChannelResolver{}
	s.userResolver = resolver

	connect := httptest.NewRequest(http.MethodPost,
		"/api/agents/"+aid+"/channels/telegram/connect",
		strings.NewReader(`{"botToken":"123456:FAKE"}`))
	connect.Header.Set("Content-Type", "application/json")
	connect.SetPathValue("id", aid)
	connect = stampAuthAndUserID(connect, uid)
	rec := httptest.NewRecorder()
	s.handleConnectAgentTelegram(rec, connect)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Channels-first: exactly one ChannelRecord hot-registered, zero
	// legacy ConfigRecord fallbacks.
	if len(resolver.records) != 1 {
		t.Fatalf("hot-registered ChannelRecords = %d; want 1 (records=%+v cfgs=%+v)",
			len(resolver.records), resolver.records, resolver.cfgs)
	}
	ch := resolver.records[0]
	if ch.Type != "telegram" || ch.AccountID != "e2e_bot" || ch.UserID != uid ||
		ch.AgentID != aid || !ch.Enabled {
		t.Errorf("hot-registered ChannelRecord = %+v; want telegram/e2e_bot (%s,%s) enabled", ch, uid, aid)
	}
	if len(resolver.cfgs) != 0 {
		t.Errorf("legacy configs fallback fired %d time(s); channels-first should win: %+v",
			len(resolver.cfgs), resolver.cfgs)
	}

	// The channels row is the row the registration was built from.
	row, err := s.dataStore.LookupChannel(context.Background(), "telegram", "e2e_bot")
	if err != nil || row == nil {
		t.Fatalf("channels row after connect: rec=%+v err=%v", row, err)
	}
}
