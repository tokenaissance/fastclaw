package agent

// The goal family's two transcript rules, and the boundary of each.
//
// 1. The user's own `/goal …` line is archived as a REAL user message. The slash short-circuit
//    returns before the turn path that normally archives an inbound, so the line existed only in
//    the web's optimistic bubble: a reload dropped it, and MCP's `read_task(full:true)` never saw
//    it — a transcript whose goal work had no goal.
// 2. A continuation is rendered as the FACT it is ((`（继续执行目标）` + the objective), role=user,
//    `synthetic: true`) — never as the audit prompt it actually carried, and only when the caller
//    asks (`includeSynthetic=1`, which the web sends and the MCP read does not).
//
// The boundaries get their own assertions, because both rules are easy to over-apply: a utility
// slash must stay silent (or every `/status` would land in the model's context), and the default
// read must keep hiding the injected rows (or §14.3's contract would have quietly changed).

import (
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

func TestSlashGoalArchivesTheUsersOwnLine(t *testing.T) {
	a := newSlashTestAgent(t)
	msg := webMsg()
	msg.Text = "/goal " + strongObjective

	res := a.handleSlashCommand(msg)
	if !res.handled {
		t.Fatalf("the goal slash was not handled: %+v", res)
	}

	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	var archived bool
	for _, m := range sess.ArchivedMessages() {
		if m.Role == "user" && m.Origin == provider.OriginUser && strings.Contains(m.Content, strongObjective) {
			archived = true
		}
	}
	if !archived {
		t.Fatalf("the user's own `/goal …` line is not in the transcript: %+v", sess.ArchivedMessages())
	}

	// And the read the web does after a reload sees it — the whole point.
	var seen bool
	for _, row := range a.WebChatHistory("chat-1", false) {
		if role, _ := row["role"].(string); role != "user" {
			continue
		}
		if text, _ := row["content"].(string); strings.Contains(text, strongObjective) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("a reload would still drop the goal line: %+v", a.WebChatHistory("chat-1", false))
	}
}

// The narrowness: a utility slash stays out of the transcript. Without this, "archive the user's
// line" reads as "archive every slash", which would put a row in the model's context for every
// glance at the machine's state.
func TestSlashUtilityCommandsAreNotArchived(t *testing.T) {
	a := newSlashTestAgent(t)
	msg := webMsg()
	msg.Text = "/help"

	if res := a.handleSlashCommand(msg); !res.handled {
		t.Fatalf("/help was not handled: %+v", res)
	}
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for _, m := range sess.ArchivedMessages() {
		if strings.Contains(m.Content, "/help") {
			t.Fatalf("a utility slash was archived into the transcript: %+v", m)
		}
	}
}

func TestWebChatHistoryRendersContinuationsOnlyWhenAsked(t *testing.T) {
	a := newSlashTestAgent(t)
	msg := webMsg()
	msg.Text = "/goal " + strongObjective
	if res := a.handleSlashCommand(msg); !res.handled {
		t.Fatalf("the goal slash was not handled: %+v", res)
	}

	// The continuation the runtime injects: archived with its own origin, exactly as the loop does.
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	const auditPrompt = "<goal_context> Only call update_goal with status=\"complete\" when every requirement is proven done. </goal_context>"
	sess.Append(provider.Message{
		Role:    "user",
		Content: auditPrompt,
		Origin:  provider.OriginGoalContext,
	})

	// Default (the MCP read): the injected row stays out, the user's own line stays in.
	plain := a.WebChatHistory("chat-1", false)
	for _, row := range plain {
		if flag, _ := row["synthetic"].(bool); flag {
			t.Fatalf("the default read rendered a synthetic row: %+v", row)
		}
	}
	if strings.Contains(renderHistory(plain), "goal_context") {
		t.Fatalf("the default read leaked the audit prompt: %s", renderHistory(plain))
	}
	if !strings.Contains(renderHistory(plain), strongObjective) {
		t.Fatalf("the default read lost the user's own goal line: %s", renderHistory(plain))
	}

	// Asked for (the web): the continuation appears as the fact, with the objective and a marker.
	withSynthetic := a.WebChatHistory("chat-1", true)
	var synthetic map[string]any
	for _, row := range withSynthetic {
		if flag, _ := row["synthetic"].(bool); flag {
			synthetic = row
		}
	}
	if synthetic == nil {
		t.Fatalf("no synthetic row was rendered: %+v", withSynthetic)
	}
	content, _ := synthetic["content"].(string)
	if !strings.Contains(content, strongObjective) {
		t.Errorf("the synthetic row does not carry the objective: %q", content)
	}
	if strings.Contains(content, "update_goal") || strings.Contains(content, "<goal_context>") {
		t.Errorf("the synthetic row carries the audit prompt instead of the fact: %q", content)
	}
	if role, _ := synthetic["role"].(string); role != "user" {
		t.Errorf("the synthetic row's role = %q, want user (the UI renders bubbles by role)", role)
	}
	if origin, _ := synthetic["origin"].(string); origin != provider.OriginGoalContext {
		t.Errorf("the synthetic row lost its origin: %q", origin)
	}
}

// renderHistory flattens a history for assertions that care about containment, not shape.
func renderHistory(rows []map[string]any) string {
	var sb strings.Builder
	for _, row := range rows {
		if s, ok := row["content"].(string); ok {
			sb.WriteString(s)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// The continuation label is the one place that decides what a continuation says, so it gets asked
// directly: an objective it could not read must not produce an invented one.
func TestGoalContinuationLabelWithoutAnObjective(t *testing.T) {
	if got := goalContinuationLabel(""); got == "" || strings.Contains(got, "goal_context") {
		t.Fatalf("an unreadable objective produced %q", got)
	}
}

// bus.SourceGoalContext is what the loop tags the injected inbound with; if that name ever moved,
// the projection above would keep working on the DB column while new rows stopped matching.
func TestGoalContextSourceIsTheOneTheLoopSets(t *testing.T) {
	if bus.SourceGoalContext != provider.OriginGoalContext {
		t.Fatalf("bus.SourceGoalContext=%q != provider.OriginGoalContext=%q — the continuation tag and the history filter have drifted",
			bus.SourceGoalContext, provider.OriginGoalContext)
	}
}
