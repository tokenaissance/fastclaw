package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

/**
 * [INPUT]: one submission's four scope parts (account, task, authorized client, key) plus
 *          the digest of its normalized instruction payload.
 * [OUTPUT]: idempotencyVerdictFor / rememberIdempotency — miss, duplicate (the first answer
 *           stands) or conflict (same key, different content: refuse loudly).
 * [POS]: The single checker §3.1 of docs/fastagent/design/14-turn-identity.md demands. It runs
 *        in the same process and the same lock-step as admission, which is what makes the
 *        promise below honest — and what makes its boundary the same boundary as the queued
 *        entry's lifetime (§14.6 path 1).
 * [PROTOCOL]: The wire field is `idempotencyKey` (`turnId` is its legacy spelling, read and
 *        nothing more). The client identity arrives as `X-Fastagent-Client`; the pod cannot
 *        derive it, because it sees the cloud account and the end user, never "which
 *        authorized MCP client". Changing any of these three names changes the contract.
 */

// The window is a promise with an edge, not a lease: it holds nothing, and outside it the same
// key is simply a new instruction (§3.1 items 3 and 8). Ten minutes is comfortably larger than
// any client's retry horizon while small enough that the map cannot grow without bound.
const idempotencyTTL = 10 * time.Minute

// Above this many live entries, a write sweeps the expired ones. A sweep on every write would
// be O(n) per request; a sweep never would let a long-lived pod accumulate one entry per
// distinct key forever. Sweeping on a rare boundary is the cheap middle, and it is enough
// because the entries expire by time, not by use.
const idempotencySweepThreshold = 512

type idempotencyEntry struct {
	digest string
	turnID string
	at     time.Time
}

type idempotencyVerdict int

const (
	idempotencyMiss idempotencyVerdict = iota
	// idempotencyDuplicate: same key, same content. It is a retry of an instruction this pod
	// already accepted, so the first turn stands and this call creates nothing.
	idempotencyDuplicate
	// idempotencyConflict: same key, different content. Never a silent overwrite — appending a
	// NEW instruction means choosing a NEW key (§3.1 items 2 and 5).
	idempotencyConflict
)

// idempotencyKeyOf is the caller's string, in either spelling. `turnId` came first and used to
// mean identity; since 2026-09-28 identity is minted here (internal/agent/turn_id.go), so the
// old spelling is read as what it should always have been — a de-duplication key.
func idempotencyKeyOf(req chatRequest) string {
	if k := strings.TrimSpace(req.IdempotencyKey); k != "" {
		return k
	}
	return strings.TrimSpace(req.TurnID)
}

// clientIdentityOf answers "which authorized client". The trust model is the same as
// X-Fastagent-End-User's: only a holder of the cloud credential can reach this handler, and
// cloud is the only thing that knows the authorized client's id. An absent header leaves the
// domain at (account, task) — narrower than §3.1 asks for, and declared as such rather than
// silently widened into "every client shares one domain".
func clientIdentityOf(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Fastagent-Client"))
}

// idempotencyScopeKey composes the four parts. `verb` keeps the operations separate: a run and
// a steer are different things to do with a task, so the same string names two different
// operations rather than colliding in one domain.
func idempotencyScopeKey(verb, uid, agentID, sessionID, client, key string) string {
	return verb + "|" + uid + "|" + agentID + "|" + sessionID + "|" + client + "|" + key
}

// instructionDigest identifies the CONTENT that a key stands for: "body + attachment summary",
// not the raw JSON bytes (§3.1 item 5 — a retry that differs only in framing is a retry). The
// parts are joined with a NUL so that ("ab", "c") and ("a", "bc") cannot collide.
func instructionDigest(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:])
}

func (s *Server) idempotencyVerdictFor(scopeKey, digest string) (idempotencyVerdict, string) {
	s.idempotencyMu.Lock()
	defer s.idempotencyMu.Unlock()
	entry, ok := s.idempotency[scopeKey]
	if !ok {
		return idempotencyMiss, ""
	}
	if time.Since(entry.at) > idempotencyTTL {
		// Expired: the same key outside the window is a new instruction, not a retry.
		delete(s.idempotency, scopeKey)
		return idempotencyMiss, ""
	}
	if entry.digest != digest {
		return idempotencyConflict, entry.turnID
	}
	return idempotencyDuplicate, entry.turnID
}

func (s *Server) rememberIdempotency(scopeKey, digest, turnID string) {
	s.idempotencyMu.Lock()
	defer s.idempotencyMu.Unlock()
	if s.idempotency == nil {
		s.idempotency = make(map[string]idempotencyEntry)
	}
	if len(s.idempotency) >= idempotencySweepThreshold {
		now := time.Now()
		for k, e := range s.idempotency {
			if now.Sub(e.at) > idempotencyTTL {
				delete(s.idempotency, k)
			}
		}
	}
	s.idempotency[scopeKey] = idempotencyEntry{digest: digest, turnID: turnID, at: time.Now()}
}
