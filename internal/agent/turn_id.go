package agent

import (
	"context"

	"github.com/google/uuid"
)

/**
 * [INPUT]: nothing; a context key local to this package, plus MintTurnID below.
 * [OUTPUT]: MintTurnID / ContextWithTurnID / TurnIDFromContext — one turn's internal
 *           identity, minted by the server at acceptance.
 * [POS]: Sibling of events.go's ContextWithChatEvents. The identity is the SERVER's
 *        (docs/fastagent/design/14-turn-identity.md §2–3, I1/I2): it is produced in the
 *        handler at the moment the submission is accepted and registered, so every turn
 *        has one — including the ones nobody's client asked for (cron, goal, heartbeat,
 *        subagent), which is exactly why the caller may not supply it. The string a
 *        client sends is a dedupe key with its own name and its own owner.
 *        Two readers, both about the submission itself — the pending-turn registry (so a
 *        queued submission can be withdrawn) and the `queued` event (so a tab that did
 *        not POST can name whose submission is waiting). It is deliberately NOT written
 *        onto the stored messages: no reader asks for it there, and a stored fact with
 *        no reader is how the removed half (2026-09-26) got written in the first place.
 * [PROTOCOL]: On change, update this header and check docs/session-turn-integrity.md
 *        (the turn facts named there) before adding a second key of this shape.
 */

type turnIDKey struct{}

// MintTurnID is the only place a turn's identity comes from: a fresh UUID, prefixed so
// that a minted id is recognisable in a log or an event body. Nothing derives it from the
// caller — "unique" is part of what the word identity means, and uniqueness cannot be
// outsourced to a party the server does not control (I2).
func MintTurnID() string { return "t_" + uuid.NewString() }

// ContextWithTurnID attaches the identity minted for this POST. Empty stays a no-op so a
// caller that has no id to give (an internal turn assembled elsewhere) behaves exactly as
// before: nothing is stamped, nothing is surfaced.
func ContextWithTurnID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, turnIDKey{}, id)
}

// TurnIDFromContext returns the id attached by ContextWithTurnID, or "".
func TurnIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(turnIDKey{}).(string)
	return id
}
