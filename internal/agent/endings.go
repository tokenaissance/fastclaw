package agent

/**
 * [INPUT]: nothing.
 * [OUTPUT]: EndingStopped / EndingFailed / EndingEmpty — the names a task read reports for how a
 *           turn ended, stamped on the terminal event that turn emits.
 * [POS]: The producer half of docs/mcp-task-submission.md §14.3's `outcome`. Before this, the
 *        three endings were only ever *prose*: a stop emitted a `notice` ("stopped at your
 *        request"), a failure an `error`, an empty model answer another `error` — and none of
 *        them was distinguishable from the others without reading the words. A reader that has
 *        to recognize a sentence is a reader that breaks when the sentence is edited, so the
 *        ending becomes a field and the prose stays what it always was: for people.
 * [PROTOCOL]: A read consults the LAST event carrying this field, and that stamp is the newest
 *        turn's ending by construction — which is why `replied` IS one of them. Without it, "no
 *        stamp" would be ambiguous between "the last turn replied" and "the last turn died
 *        before it could say", and a reader would attribute an older stamp to a newer turn.
 */

const (
	// EndingReplied: the turn finished and delivered text. It is stamped on the closing event
	// (not inferred from the archive) so that "which turn does this ending belong to?" has one
	// answer for every ending — the newest stamp is the newest turn's.
	EndingReplied = "replied"
	// EndingStopped: the turn stopped because someone asked it to (or a peer took the session
	// over). Both are "this turn did not finish", which is the caller-facing question.
	EndingStopped = "stopped"
	// EndingFailed: a provider/LLM failure ended the turn.
	EndingFailed = "failed"
	// EndingEmpty: the turn finished and produced no text. Distinct from `failed` on purpose —
	// the action a reader should take is "ask again", not "report an incident".
	EndingEmpty = "empty"
)
