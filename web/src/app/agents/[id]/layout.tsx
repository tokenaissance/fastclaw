import AgentAccessGate from "@/components/agent-access-gate";

export function generateStaticParams() {
  return [{ id: "default" }];
}

// Server params here resolve at BUILD time (output: 'export' bakes
// generateStaticParams' "default" into the bundle), so we can't pass
// the agent id down — the gate reads it from the URL on the client
// via useParams().
export default function AgentLayout({ children }: { children: React.ReactNode }) {
  return <AgentAccessGate>{children}</AgentAccessGate>;
}
