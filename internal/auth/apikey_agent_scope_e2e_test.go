package auth

// The account boundary of the MCP surface, pinned on the pod side.
//
// The cloud half (mcp-oauth-server) decides which agent a call is about by matching the
// requested id against the AGENTS OF THE TOKEN'S ACCOUNT. That is not a proof on its own —
// it is a list comparison — so the pod re-decides from the credential it was handed, and
// this file pins what that second judgement actually grants:
//
//   - a type=user key (the one cloud provisioning mints for every account, see
//     fastagent-provisioning.ts `createApiKeyForUser`) reaches exactly the agents owned by
//     the key's owner, resolved at request time;
//   - it does not reach another account's agent, and the mirror case is asserted too, so the
//     test cannot pass by granting nothing at all;
//   - an agent the owner creates AFTER the key was minted is in scope without touching the
//     key, which is the "no ACL maintenance for new agents" half of that tier;
//   - and a type=agent key stays an explicit list. Without this last case, "the user tier
//     resolves per request" would be indistinguishable from "every tier does", and the
//     boundary between the two tiers would be unpinned.
//
// The chain this closes, end to end: cloud token (audience + scope) -> the account's own
// fastagent key -> the pod's per-request ownership resolution. The stricter of the two
// halves wins, and neither half alone is the invariant.

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

func TestAPIKeyScope_UserKeySeesOnlyItsOwnersAgents(t *testing.T) {
	resolver, st, accts, keys := newIdentityEnv(t)
	ctx := context.Background()

	alice, _, aliceToken := createOwnerWithKey(t, accts, keys, "scope-alice")
	bob, _, bobToken := createOwnerWithKey(t, accts, keys, "scope-bob")

	seedAgent := func(ownerID, agentID, name string) {
		t.Helper()
		if err := st.SaveAgent(ctx, &store.AgentRecord{
			ID: agentID, UserID: ownerID, Name: name,
		}); err != nil {
			t.Fatalf("seed agent %s: %v", agentID, err)
		}
	}
	seedAgent(alice, "agt_alice_1", "Alice one")
	seedAgent(bob, "agt_bob_1", "Bob one")

	identA, err := resolver.ResolveBearer(ctx, aliceToken)
	if err != nil {
		t.Fatalf("resolve alice's key: %v", err)
	}
	if identA.APIKeyType != users.APIKeyTypeUser {
		t.Fatalf("the fixture minted a %q key; this witness is about the type=user tier", identA.APIKeyType)
	}
	if !identA.CanAccessAgent("agt_alice_1") {
		t.Errorf("a user key cannot reach its own account's agent - the invariant would be vacuously true")
	}
	if identA.CanAccessAgent("agt_bob_1") {
		t.Errorf("a user key reached ANOTHER account's agent (agt_bob_1): the pod's half of the MCP boundary is open")
	}

	// The mirror, so a failure mode that grants nothing cannot pass for a boundary.
	identB, err := resolver.ResolveBearer(ctx, bobToken)
	if err != nil {
		t.Fatalf("resolve bob's key: %v", err)
	}
	if !identB.CanAccessAgent("agt_bob_1") {
		t.Errorf("bob's user key cannot reach bob's own agent")
	}
	if identB.CanAccessAgent("agt_alice_1") {
		t.Errorf("bob's user key reached alice's agent: the second half is not symmetric")
	}

	// An agent created after the key was minted: in scope, without touching the key. This is
	// what "every agent owned by the key's owner, resolved per request" buys.
	seedAgent(alice, "agt_alice_2", "Alice two")
	again, err := resolver.ResolveBearer(ctx, aliceToken)
	if err != nil {
		t.Fatalf("resolve alice's key again: %v", err)
	}
	if !again.CanAccessAgent("agt_alice_2") {
		t.Errorf("the user tier did not grow with the account: a newly created agent is unreachable")
	}
	if again.CanAccessAgent("agt_bob_1") {
		t.Errorf("the re-resolved key reached another account")
	}

	// The boundary between the tiers: type=agent is an explicit ACL and does NOT grow.
	_, aclToken, err := keys.Create(ctx, alice, "alice-explicit", users.APIKeyTypeAgent, []string{"agt_alice_1"})
	if err != nil {
		t.Fatalf("create an ACL key: %v", err)
	}
	identACL, err := resolver.ResolveBearer(ctx, aclToken)
	if err != nil {
		t.Fatalf("resolve the ACL key: %v", err)
	}
	if !identACL.CanAccessAgent("agt_alice_1") {
		t.Errorf("an explicit ACL key lost the agent it was issued for")
	}
	if identACL.CanAccessAgent("agt_alice_2") {
		t.Errorf("a type=agent key grew with the account: its ACL is explicit by definition")
	}
}
