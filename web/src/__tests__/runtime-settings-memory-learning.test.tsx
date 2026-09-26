/**
 * The writer half of the two switch rows that had none.
 *
 * `memory.autoPersist` and `skillsLearner` are read at agent-build time
 * (fastagent register rows 52 and 51), and until this page grew a form their
 * only writer was a hand-made POST /api/config. Register row 52's own audit is
 * what surfaced it: the reader chain was witnessed end to end, and the switch
 * an operator would look for did not exist.
 *
 * What is pinned here is the payload shape this page sends — the namespaces,
 * the cadence floor encoded as 0 ("unset" on the wire, the runtime then stamps
 * its documented default) and the off-switch. The Go side
 * (`internal/setup/runtime_settings_memory_learning_e2e_test.go`) pins that the
 * same body, through the real handler, lands at system scope and reads back
 * through the same typed call the gateway uses.
 */
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// One stable router object: the page's load effect is keyed on `[router]`, and
// a mock that hands back a fresh object per render re-runs that effect on every
// state change, silently resetting whatever the test just typed.
const nav = vi.hoisted(() => ({
  router: { push: vi.fn(), replace: vi.fn(), prefetch: vi.fn() },
}));

vi.mock("next/navigation", () => ({ useRouter: () => nav.router }));

const api = vi.hoisted(() => ({
  getMe: vi.fn(),
  getConfig: vi.fn(),
  updateConfig: vi.fn(),
}));

vi.mock("@/lib/api", () => api);

import RuntimeSettingsPage from "@/app/settings/runtime/page";

// The shape the page sends to POST /api/config: the namespaces it owns, plus
// the two this test is about.
type UpdatePayload = {
  prefs?: Record<string, unknown>;
  sandbox?: Record<string, unknown>;
  memory: { autoPersist: { enabled: boolean; everyNTurns: number; model?: string } };
  skillsLearner: { enabled: boolean; minToolCalls: number; model?: string };
  privacy: { piiScrubbing: { enabled: boolean } };
};

const baseConfig = (overrides: Record<string, unknown> = {}) => ({
  providers: {},
  agents: { defaults: { model: "m", maxTokens: 1, temperature: 0.1, maxToolIterations: 1 } },
  channels: {},
  storage: { type: "sqlite" },
  hooks: { enabled: false },
  prefs: { timezone: "" },
  sandbox: { enabled: false },
  ...overrides,
});

describe("runtime settings — memory auto-persist and the skills learner", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    api.getMe.mockResolvedValue({ user: { role: "super_admin" } });
    api.updateConfig.mockResolvedValue({ ok: true });
  });

  it("renders the stored rows, including the cadence and the extractor model", async () => {
    api.getConfig.mockResolvedValue(
      baseConfig({
        memory: { autoPersist: { enabled: true, everyNTurns: 7, model: "p/distill" } },
        skillsLearner: { enabled: true, minToolCalls: 4, model: "p/extract" },
        privacy: { piiScrubbing: { enabled: true } },
      }),
    );

    render(<RuntimeSettingsPage />);

    await waitFor(() =>
      expect(screen.getByLabelText("Every N chatter turns")).toHaveValue(7),
    );
    expect(screen.getByLabelText("Distill model (optional)")).toHaveValue("p/distill");
    expect(screen.getByLabelText("Minimum tool calls")).toHaveValue(4);
    expect(screen.getByLabelText("Extraction model (optional)")).toHaveValue("p/extract");
    expect(screen.getByRole("switch", { name: "Memory auto-persist" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Skills learner" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Redact PII before the model" })).toBeChecked();
  });

  it("an untouched save sends both namespaces with the stored numbers", async () => {
    api.getConfig.mockResolvedValue(
      baseConfig({
        memory: { autoPersist: { enabled: true, everyNTurns: 7 } },
        skillsLearner: { enabled: true, minToolCalls: 4 },
        privacy: { piiScrubbing: { enabled: true } },
      }),
    );

    render(<RuntimeSettingsPage />);
    await waitFor(() => expect(screen.getByRole("button", { name: /save/i })).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(api.updateConfig).toHaveBeenCalledTimes(1));
    const payload = api.updateConfig.mock.calls[0][0] as UpdatePayload;
    expect(payload.memory.autoPersist).toEqual({
      enabled: true,
      everyNTurns: 7,
      model: '',
    });
    expect(payload.skillsLearner).toEqual({
      enabled: true,
      minToolCalls: 4,
      model: '',
    });
    expect(payload.privacy).toEqual({ piiScrubbing: { enabled: true } });
    // The namespaces this page already owned must still be in the patch — the
    // handler is a PATCH, and a payload that dropped them would look saved.
    expect(payload.prefs).toBeDefined();
    expect(payload.sandbox).toBeDefined();
  });

  it("an emptied cadence or floor resets to the runtime default (0 on the wire)", async () => {
    api.getConfig.mockResolvedValue(
      baseConfig({
        memory: { autoPersist: { enabled: true, everyNTurns: 7 } },
        skillsLearner: { enabled: true, minToolCalls: 4 },
      }),
    );

    render(<RuntimeSettingsPage />);
    const cadence = await screen.findByLabelText("Every N chatter turns");
    await userEvent.clear(cadence);
    await userEvent.clear(screen.getByLabelText("Minimum tool calls"));
    await userEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(api.updateConfig).toHaveBeenCalledTimes(1));
    const payload = api.updateConfig.mock.calls[0][0] as UpdatePayload;
    expect(payload.memory.autoPersist.everyNTurns).toBe(0);
    expect(payload.skillsLearner.minToolCalls).toBe(0);
  });

  it("an operator can turn a row off, and the off value is written", async () => {
    api.getConfig.mockResolvedValue(
      baseConfig({
        memory: { autoPersist: { enabled: false } },
        skillsLearner: { enabled: true },
        privacy: { piiScrubbing: { enabled: true } },
      }),
    );

    render(<RuntimeSettingsPage />);
    const learner = await screen.findByRole("switch", { name: "Skills learner" });
    await userEvent.click(learner);
    await userEvent.click(screen.getByRole("switch", { name: "Redact PII before the model" }));
    await userEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(api.updateConfig).toHaveBeenCalledTimes(1));
    const payload = api.updateConfig.mock.calls[0][0] as UpdatePayload;
    expect(payload.skillsLearner.enabled).toBe(false);
    expect(payload.memory.autoPersist.enabled).toBe(false);
    expect(payload.privacy.piiScrubbing.enabled).toBe(false);
  });
});
