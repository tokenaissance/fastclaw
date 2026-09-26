package agent

import "context"

/**
 * [INPUT]: nothing; a context key local to this package.
 * [OUTPUT]: ContextWithTurnID / TurnIDFromContext — carry the id the *client* gave
 *           for one POST from the HTTP handler down to the code that answers about
 *           that submission.
 * [POS]: Sibling of events.go's ContextWithChatEvents, and narrower than a turn
 *        *identity*: this is the client's request id. Two readers, both about the
 *        submission itself — the pending-turn registry (so a queued submission can
 *        be withdrawn) and the `queued` event (so a tab that did not POST can name
 *        whose submission is waiting). Nothing else reads it, and it is deliberately
 *        NOT written onto the stored messages (that half was removed on 2026-09-26:
 *        it existed only for an MCP read contract that is still being redesigned).
 * [PROTOCOL]: On change, update this header and check docs/session-turn-integrity.md
 *        (the turn facts named there) before adding a second key of this shape.
 */

type turnIDKey struct{}

// ContextWithTurnID attaches the id the client generated for this POST
// (`chatRequest.TurnID`). Empty is a no-op: a submission without one keeps
// today's behaviour exactly (nothing is stamped, nothing is surfaced).
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
