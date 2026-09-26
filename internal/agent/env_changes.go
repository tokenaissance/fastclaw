package agent

// The unified environment-change signal.
//
// docs/文件系统形式化证明/08-state-observability-principle.md states the
// rule this file implements: every change to the agent's world that the agent
// did not cause itself has to be perceivable, in a channel the agent actually
// reads. Three subsystems violated it in the same way — each one silently
// replaced something between turns:
//
//   - context compaction replaced earlier history with a summary;
//   - the background memory review rewrote MEMORY.md;
//   - the per-turn skill refresh could add or REMOVE skills.
//
// The third case is the hard one, and the reason this is one shared exit rather
// than three subsystem-specific messages: a NEW thing is often noticed by
// accident (it appears in the prompt), while a REMOVED thing leaves no trace —
// the agent's default assumption is that the world is unchanged. So this exit
// states the delta against the previous turn, for everything that can disappear.
//
// It is an exception channel (C3): no changes, no signal. And it is silent on the
// first turn of a session, because "first observation" is not a change — there
// is nothing to compare against and claiming otherwise would be a fabricated
// fact.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// envSnapshot is the agent's world at one point in time, reduced to the parts
// that can change without the agent doing anything.
//
// It is also the document that travels in the turn receipt: the sampler encodes
// one per turn, the next turn decodes it and diffs. That is what makes the
// signal survive a rebuilt agent, a restart or another replica — the baseline
// is a durable record written by the turn that produced it, not state this
// process keeps (docs 10 §4, G20).
type envSnapshot struct {
	// skills maps a skill name to a cheap content fingerprint (layer +
	// description). Enough to notice an edit without reading the file.
	skills map[string]string
	// tools is the set of registered tool names.
	tools map[string]bool
	// memoryHash is the sha256 of the loaded long-term memory content ("" when
	// there is none). Content, not mtime: the background review rewrites the
	// file, and a rewrite with identical content is not a change worth naming.
	memoryHash string
	// skillsIncomplete records that the object store could not be read while
	// building the skill list, so the set above may be MISSING entries. Kept as
	// its own field rather than folded into `skills` because it is not a
	// difference between two snapshots — it is a statement about the current
	// one's trustworthiness, and it must be stated even on the first turn.
	skillsIncomplete bool
	// identity maps each identity file's name to a fingerprint of the content
	// the system prompt would carry for this chatter ("" when the file is absent
	// or empty). These files reach the prompt on every turn, so a change made
	// from outside this conversation rewrites part of what the agent believes
	// about itself — and until 2026-09-18 nothing said so (docs 10 §3.3, G8).
	identity map[string]string
	// config is the part of the agent's own configuration that visibly shapes
	// what it can do (model, prompt mode). A change here is a change to its
	// world, in the same family as a changed skill or tool set (docs 10 §3.3, G9).
	config string
	// cron maps a scheduled job's id to its definition. Run bookkeeping
	// (LastRun / NextRun / FailureCount) is deliberately excluded: the scheduler
	// writes it on every tick, and including it would fire this signal every
	// turn (C3). What it catches is a job created, edited or DELETED from
	// outside — after which "I will be woken at 9" is a false belief
	// (docs 10 §3.3, G10).
	cron map[string]string
	// cronIncomplete records that the job list could not be read, so `cron` may be
	// missing entries. Same role as skillsIncomplete: a statement about this
	// snapshot's trustworthiness, rather than a change between snapshots.
	cronIncomplete bool
}

// renderEnvDelta renders one turn's delta as the σ the agent reads (docs 08 §2).
// Empty string means "nothing to say", which covers both "nothing changed" and
// "nothing to compare against yet".
//
// Pure on purpose. The previous snapshot is not this process's memory of the
// last turn — it is decoded from the conversation's own receipt, so the
// comparison is unaffected by which replica answers, whether the agent was
// rebuilt, or how long the conversation was idle (docs 10 §4, G9 + G20).
func renderEnvDelta(prev envSnapshot, seen bool, cur envSnapshot) string {
	var lines []string

	// Not a difference between snapshots: a statement about whether THIS one can
	// be trusted. Stated even on the first turn, because "I could not load
	// your skills" is exactly the fact that stops the agent from denying a
	// capability it actually has.
	if cur.skillsIncomplete {
		lines = append(lines, "- your skill list could NOT be fully loaded this turn (the skill store could not be read): "+
			"it may be missing skills you have. Do not conclude from this list alone that a capability is unavailable — say the list is incomplete instead.")
	}
	if cur.cronIncomplete {
		lines = append(lines, "- your scheduled-job list could NOT be read this turn (the store read failed): "+
			"it may be missing jobs you have. Do not conclude from this list alone that a job was deleted — say the list is incomplete instead.")
	}

	if seen {
		// missingFrom(a, b) = the keys of a that b lacks. Removed means "was
		// there, is not any more" — the direction matters, and getting it
		// backwards makes the signal state additions as removals and vice versa.
		if removed := missingFrom(prev.skills, cur.skills); len(removed) > 0 {
			lines = append(lines, "- skills removed: "+strings.Join(removed, ", "))
		}
		if added := missingFrom(cur.skills, prev.skills); len(added) > 0 {
			lines = append(lines, "- skills added: "+strings.Join(added, ", "))
		}
		if changed := changedBetween(prev.skills, cur.skills); len(changed) > 0 {
			lines = append(lines, "- skills changed: "+strings.Join(changed, ", "))
		}
		if prev.memoryHash != cur.memoryHash {
			switch {
			case prev.memoryHash == "":
				lines = append(lines, "- long-term memory was created")
			case cur.memoryHash == "":
				lines = append(lines, "- long-term memory was CLEARED")
			default:
				lines = append(lines, "- long-term memory was rewritten (it may say something different now)")
			}
		}
		if removed := missingFromBool(prev.tools, cur.tools); len(removed) > 0 {
			lines = append(lines, "- tools no longer available: "+strings.Join(removed, ", "))
		}
		if added := missingFromBool(cur.tools, prev.tools); len(added) > 0 {
			lines = append(lines, "- tools now available: "+strings.Join(added, ", "))
		}
		// Identity files carry the agent's own definition (SOUL / IDENTITY /
		// USER / …). They are read fresh into every prompt, so an edit made from
		// outside this conversation — the panel, another session, another pod —
		// rewrites what the agent believes about itself with no other trace
		// (docs 10 §3.3, G8).
		if changed := changedStringMaps(prev.identity, cur.identity); len(changed) > 0 {
			lines = append(lines, "- identity files changed: "+strings.Join(changed, ", ")+
				" (the system prompt no longer says what it did — re-read them before relying on them)")
		}
		// A config change that reaches a live agent (a reload rebuilds the agent,
		// and the rebuilt agent's baseline comes from the conversation's receipt,
		// not from its own memory — see configFingerprint).
		if prev.config != "" && prev.config != cur.config {
			lines = append(lines, "- my configuration changed: "+prev.config+" → "+cur.config)
		}
		// Scheduled jobs decide when the agent wakes up on its own. "It will
		// happen at 9" is a belief about the world, so a job that vanished or
		// moved has to be stated like any other change (docs 10 §3.3, G10).
		// Skipped when either side could not be read: a failed read must not be
		// reported as a deletion.
		if !prev.cronIncomplete && !cur.cronIncomplete {
			if gone := missingFrom(prev.cron, cur.cron); len(gone) > 0 {
				lines = append(lines, "- scheduled jobs no longer exist: "+strings.Join(cronLabels(prev.cron, gone), ", "))
			}
			if added := missingFrom(cur.cron, prev.cron); len(added) > 0 {
				lines = append(lines, "- scheduled jobs added: "+strings.Join(cronLabels(cur.cron, added), ", "))
			}
			if moved := changedBetween(prev.cron, cur.cron); len(moved) > 0 {
				lines = append(lines, "- scheduled jobs changed: "+strings.Join(cronLabels(cur.cron, moved), ", "))
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}
	return "[Environment changes since your last turn — a fact about your world, not an instruction]\n" +
		strings.Join(lines, "\n") + "\n" +
		"Removed items are gone, not hidden: if your plan depended on one, re-check with your tools before continuing."
}

// missingFrom returns the keys of a that b does not have, sorted.
func missingFrom(a, b map[string]string) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// changedBetween returns the keys present in both whose fingerprint differs.
func changedBetween(prev, cur map[string]string) []string {
	var out []string
	for k, v := range cur {
		if before, ok := prev[k]; ok && before != v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// missingFromBool is missingFrom for sets.
func missingFromBool(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// changedStringMaps returns the keys whose value differs between two maps:
// added, removed and edited all count, because from the reader's side a file
// that changed, appeared or vanished reads differently in every case.
func changedStringMaps(prev, cur map[string]string) []string {
	var out []string
	for k, v := range cur {
		if before, ok := prev[k]; !ok || before != v {
			out = append(out, k)
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// cronFingerprint reduces scheduled jobs to id → definition. It is a pure
// function of the rows so the exclusion below is testable: LastRun, NextRun and
// FailureCount are the scheduler's bookkeeping and change on every tick, while
// name / schedule / type / enabled are what an operator would edit or delete —
// exactly the changes that turn "I will be woken at 9" into a false belief.
func cronFingerprint(jobs []store.CronJobRecord) map[string]string {
	out := make(map[string]string, len(jobs))
	for _, j := range jobs {
		out[j.ID] = fmt.Sprintf("%s|%s|%s|enabled=%t", j.Name, j.Schedule, j.Type, j.Enabled)
	}
	return out
}

// cronLabels renders job ids as their names ("morning"), falling back to the id
// when a job has no name. The agent reasons with names; the id stays available
// in the tool surface for anything that needs it.
func cronLabels(m map[string]string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name := m[id]
		if i := strings.IndexByte(name, '|'); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			name = id
		}
		out = append(out, name)
	}
	return out
}

// hashMemory reduces memory content to the fingerprint the tracker compares.
func hashMemory(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// identitySampleFiles is the set whose content reaches the system prompt on
// every turn (the agent-mode bootstrap list of prompt_modules.go; the other
// prompt modes carry a subset of it).
var identitySampleFiles = []string{
	"SOUL.md", "IDENTITY.md", "USER.md", "BOOTSTRAP.md", "AGENTS.md", "HEARTBEAT.md", "TOOLS.md",
}

// identityFingerprints reads the identity files the way the prompt builder does
// and reduces each to a fingerprint: names are reported, contents never are.
// The extra reads are the price of the signal, and they are the same set the
// prompt itself is about to read (docs 10 §3.3, G8).
func (a *Agent) identityFingerprints(chatterUID string) map[string]string {
	if a.ctxBuilder == nil {
		return nil
	}
	out := make(map[string]string, len(identitySampleFiles))
	for _, name := range identitySampleFiles {
		out[name] = hashMemory(a.ctxBuilder.loadFileForUser(name, chatterUID))
	}
	return out
}

// configFingerprint reduces the agent's configuration to one comparable string:
// the model and the prompt mode both visibly shape what it can do and what its
// prompt looks like.
//
// A configuration change made through the admin surface rebuilds the cached
// UserSpace (and therefore this Agent), so the in-process tracker starts with no
// previous snapshot. That used to make the change invisible for good. What lets
// the rebuilt agent state it anyway is not a baseline of its own: it is the
// conversation's own turn receipt (configBaselineFromReceipt), which already
// records what this conversation last ran under (docs 10 §4, G9).
func (a *Agent) configFingerprint() string {
	mode := ""
	if a.ctxBuilder != nil {
		mode = a.ctxBuilder.promptMode
	}
	return "model=" + a.model + " prompt_mode=" + mode
}

// encodeEnvSnapshot renders one turn's world as the document the turn receipt
// carries. It is deliberately opaque to the session package: the producer and
// the reader are both here (docs 10 §4, G20).
func encodeEnvSnapshot(s envSnapshot) string {
	raw, err := json.Marshal(docFromSnapshot(s))
	if err != nil {
		// Cannot happen for this shape, but a receipt is a diagnostic: a silent
		// failure would turn into "everything changed" on the next turn.
		slog.Warn("environment signal: could not encode the turn receipt", "error", err)
		return ""
	}
	return string(raw)
}

// envBaselineFromReceipt returns the world this conversation last ran in, read
// from the conversation's OWN turn receipts.
//
// The receipt exists already: session.Session.Append stamps every assistant
// message with the model that produced it (provider/model columns) and with the
// document the sampler produced (metadata), and both are durable in
// session_messages. So the environment signal needs no baseline of its own — no
// extra row, no extra write, nothing to clean up — while still satisfying the
// duty that made the baseline durable in the first place: a config change takes
// effect by rebuilding the agent, a restart or another replica can serve the
// next turn, and the fact it must be compared against has to outlive all three
// (docs 08 §2.2 O4; docs 10 §4, G9 + G20).
//
// Only an explicit, decodable receipt counts. A missing one is "not sampled",
// never a default: the first turn of a conversation, a history rewritten by
// compaction, or rows written before the stamp existed. Silence is the same
// rule every other first observation follows, and a fabricated baseline would
// make every turn announce changes that never happened.
//
// The LAST stamped assistant message wins: it is the most recent world this
// conversation actually saw itself run in.
func envBaselineFromReceipt(history []provider.Message) (envSnapshot, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		raw := session.RunReceiptOf(history[i])
		if raw == "" {
			continue
		}
		var doc envSnapshotDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			slog.Warn("environment signal: a turn receipt could not be decoded; staying quiet rather than guessing",
				"error", err)
			return envSnapshot{}, false
		}
		return doc.snapshot(), true
	}
	return envSnapshot{}, false
}

// envSnapshotDoc is the wire shape of a receipt.
//
// It exists so the document has a declared, exported shape (what a reader of a
// stored receipt sees) while the in-package snapshot keeps unexported fields:
// the alternative — exporting the snapshot — would let every subsystem poke at
// it, and encoding/json cannot tag unexported fields. The conversion pair is also
// where a future field's wire name is decided exactly once.
type envSnapshotDoc struct {
	Skills           map[string]string `json:"skills,omitempty"`
	Tools            map[string]bool   `json:"tools,omitempty"`
	Memory           string            `json:"memory,omitempty"`
	SkillsIncomplete bool              `json:"skills_incomplete,omitempty"`
	Identity         map[string]string `json:"identity,omitempty"`
	Config           string            `json:"config,omitempty"`
	Cron             map[string]string `json:"cron,omitempty"`
	CronIncomplete   bool              `json:"cron_incomplete,omitempty"`
}

func docFromSnapshot(s envSnapshot) envSnapshotDoc {
	return envSnapshotDoc{
		Skills:           s.skills,
		Tools:            s.tools,
		Memory:           s.memoryHash,
		SkillsIncomplete: s.skillsIncomplete,
		Identity:         s.identity,
		Config:           s.config,
		Cron:             s.cron,
		CronIncomplete:   s.cronIncomplete,
	}
}

func (d envSnapshotDoc) snapshot() envSnapshot {
	s := envSnapshot{
		skills:           d.Skills,
		tools:            d.Tools,
		memoryHash:       d.Memory,
		skillsIncomplete: d.SkillsIncomplete,
		identity:         d.Identity,
		config:           d.Config,
		cron:             d.Cron,
		cronIncomplete:   d.CronIncomplete,
	}
	// The diff helpers index these maps; nil is fine for reading, but keeping
	// the shape non-nil makes "absent" and "empty" read the same way they do on
	// a freshly sampled snapshot.
	if s.skills == nil {
		s.skills = map[string]string{}
	}
	if s.tools == nil {
		s.tools = map[string]bool{}
	}
	return s
}

// cronFingerprints samples this agent's scheduled jobs. A job the agent created
// itself already came back as its own receipt; what this catches is a job
// created, edited or deleted from outside the conversation — the panel, another
// session, an operator (docs 10 §3.3, G10).
//
// ok=false means "not sampled" (no relational store, or the read failed). The
// caller must then skip the diff rather than treat nil as an empty list: a
// failed read reported as "your jobs are gone" would be the same class of error
// this whole document is about.
//
// The sample is scoped to chatterUID: the job list is per-agent data, but
// what a chatter may learn about it is not — one chatter's job names must
// not arrive in another chatter's context as "scheduled jobs added". The
// agent owner keeps the agent-wide view, matching the tool's gate.
func (a *Agent) cronFingerprints(chatterUID string) (map[string]string, bool) {
	if a.dataStore == nil {
		return nil, false
	}
	jobs, err := a.dataStore.ListCronJobsByAgent(context.Background(), a.name)
	if err != nil {
		slog.Warn("environment signal: cron job list unreadable", "agent", a.name, "error", err)
		return nil, false
	}
	if chatterUID != "" && chatterUID != a.ownerUserID {
		visible := make([]store.CronJobRecord, 0, len(jobs))
		for _, j := range jobs {
			if j.Creator() == chatterUID {
				visible = append(visible, j)
			}
		}
		jobs = visible
	}
	return cronFingerprint(jobs), true
}

// signalEnvironmentChanges samples the agent's world and hands the resulting σ
// to the context builder — the turn-level exit of docs 08 §9.1. Called once per
// turn, right after the skill refresh: the single point where every
// between-turn subsystem is already being read.
//
// Nothing here fails a turn: if a subsystem cannot be sampled (a store read
// error, a nil dependency), that part of the snapshot is simply absent, which
// can only make the signal quieter. Under-stating is a real cost (C2), but so is
// breaking a turn over a diagnostic — and the subsystems themselves already log
// their own failures.
// The returned string is the receipt for THIS turn: the loop stamps it onto the
// assistant message it is about to produce, so the next turn (any replica, any
// instance) can diff against it. Empty when there is nothing to record.
func (a *Agent) signalEnvironmentChanges(chatterUID, chatID string, history []provider.Message) string {
	if a.ctxBuilder == nil {
		return ""
	}
	cur := envSnapshot{
		skills:           map[string]string{},
		tools:            map[string]bool{},
		skillsIncomplete: a.skillsHydrationFailed,
	}
	cur.identity = a.identityFingerprints(chatterUID)
	cur.config = a.configFingerprint()
	if jobs, ok := a.cronFingerprints(chatterUID); ok {
		cur.cron = jobs
	} else if a.dataStore != nil {
		cur.cronIncomplete = true
	}
	if a.registry != nil {
		for _, name := range a.registry.ToolNames() {
			cur.tools[name] = true
		}
	}
	if a.skillsFingerprint != nil {
		cur.skills = a.skillsFingerprint
	}
	if a.memory != nil {
		mem := a.memory
		if chatterUID != "" {
			mem = a.memory.WithUserID(chatterUID)
		}
		cur.memoryHash = hashMemory(mem.LoadMemory())
	}
	// The previous world comes from the conversation's own receipts (durable,
	// per conversation, written by the turn that saw it), so nothing about this
	// comparison depends on this instance still being alive (docs 10 §4, G20).
	prev, seen := envBaselineFromReceipt(history)
	a.ctxBuilder.SetEnvironmentSignal(renderEnvDelta(prev, seen, cur))
	// Hand the sampled world back so the turn stamps it onto its own receipt —
	// the next turn's baseline. The loop owns that write because the receipt
	// rides the assistant message (session.Append), the same place the model
	// that produced the reply is recorded.
	return encodeEnvSnapshot(cur)
}

// String renders a snapshot for tests and logs. Unused fields are shown as
// counts so a diff of two snapshots reads as a sentence.
func (s envSnapshot) String() string {
	return fmt.Sprintf("skills=%d tools=%d memory=%t", len(s.skills), len(s.tools), s.memoryHash != "")
}
