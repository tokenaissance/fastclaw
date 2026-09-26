package setup

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// chatEventWriter renders one event record into one SSE frame, and owns the
// cursor that keeps the three sources — connect replay, the in-process hub, and
// the store tail — from sending the same event twice.
//
// One writer, one cursor. The duplicate it prevents is real and racy: the tail
// reads a range and, before it finishes writing that range, the hub can deliver
// the same seqs; without the cursor the client would receive both copies (the
// browser drops the duplicate by seq, but the wire carries it twice, and any
// consumer without that guard would render it twice).
type chatEventWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	since   int64
}

func newChatEventWriter(w http.ResponseWriter, flusher http.Flusher, since int64) *chatEventWriter {
	return &chatEventWriter{w: w, flusher: flusher, since: since}
}

// cursor is the highest seq delivered, and the value every subsequent query
// passes as `since`.
func (cw *chatEventWriter) cursor() int64 { return cw.since }

// shouldSend is the whole de-duplication rule: unknown (negative) seqs are
// live-only and are never compared; a known seq must be strictly newer than the
// cursor. Strictly-newer also drops the out-of-order case above.
func (cw *chatEventWriter) shouldSend(seq int64) bool {
	return seq < 0 || seq > cw.since
}

// emit writes one SSE frame. `id:` is only sent for persisted events: it is what
// the browser sends back as Last-Event-ID, and a live-only event must not become
// somebody's resume point.
func (cw *chatEventWriter) emit(seq int64, typ string, data []byte) {
	if !cw.shouldSend(seq) {
		return
	}
	if seq >= 0 {
		cw.since = seq
		fmt.Fprintf(cw.w, "id: %d\n", seq)
	}
	payload := map[string]any{
		"seq":  seq,
		"type": typ,
	}
	if len(data) > 0 && string(data) != "null" {
		payload["data"] = json.RawMessage(data)
	}
	line, _ := json.Marshal(payload)
	fmt.Fprintf(cw.w, "data: %s\n\n", line)
	cw.flusher.Flush()
}

// emitPersisted renders the rows that arrived from the LOG — the two readers of it
// are the connect replay and the tail, and they must answer this question the same
// way, so it is answered once, here.
//
// A live-only type never travels on this path: the hub is its only transport, and a
// copy replayed out of the store would be a second expression of one live stream,
// which no cursor can dedupe (those events carry seq = -1 by definition). The tail
// has always said that; the replay did not, and the gap was reachable — measured
// 2026-09-26 as an intermittent CI failure, see isLiveOnlyEventType's note.
func (cw *chatEventWriter) emitPersisted(rows []store.SessionEventRecord) {
	for _, rec := range rows {
		if isLiveOnlyEventType(rec.Type) {
			continue
		}
		cw.emit(rec.Seq, rec.Type, rec.Data)
	}
}
