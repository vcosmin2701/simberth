// The agent runner drives ONE simulator for the length of a run.
//
// The Go orchestrator spawns one of these per leased simulator and reads its
// stdout as NDJSON. Every line is a simberth Event, matching run.go exactly —
// that struct is the contract shared by this file, the orchestrator, and the
// macOS app.
//
// The simulator tools are an in-process SDK MCP server whose handlers close
// over a single UDID. An agent therefore cannot reach another agent's
// simulator: the isolation is structural, not a matter of the model behaving.

import { query, tool, createSdkMcpServer } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { join } from "node:path";

const execFileAsync = promisify(execFile);

const AXE = process.env.SIMBERTH_AXE || "axe";
const SIMBERTH = process.env.SIMBERTH_CLI || "simberth";

// ---------------------------------------------------------------- event stream

let stepCounter = 0;
const recorded = []; // acting steps, for the replay file

function emit(event) {
  // One event per line, flushed immediately: the GUI shows steps live, so
  // buffering here would make the UI lag behind the simulator.
  process.stdout.write(JSON.stringify({ at: Date.now(), ...event }) + "\n");
}

// ------------------------------------------------------------------ simulator

async function axe(args) {
  const { stdout } = await execFileAsync(AXE, args, { maxBuffer: 64 * 1024 * 1024 });
  return stdout;
}

// describeUI goes through the simberth CLI rather than AXe directly so the
// distillation lives in exactly one place (axe.go). The raw tree is ~75x
// larger and would dominate the agent's context on every turn.
async function describeUI(udid) {
  const { stdout } = await execFileAsync(SIMBERTH, ["ui", "describe", udid, "--json"], {
    maxBuffer: 64 * 1024 * 1024,
  });
  return JSON.parse(stdout);
}

/**
 * Wraps a tool handler so every call becomes a visible step: a start event, a
 * screenshot, a finish event with its duration, and — for acting tools — an
 * entry in the replay file.
 */
function step(udid, runId, simName, runDir, tool_, summarize, replayOf, fn) {
  return async (args) => {
    const id = ++stepCounter;
    const summary = summarize(args);
    emit({ type: "step.started", runId, udid, simName, stepId: id, tool: tool_, summary });

    const started = Date.now();
    try {
      const result = await fn(args);

      // A screenshot per step is what makes a finished run reviewable.
      let shot = "";
      try {
        shot = join("screenshots", `${udid}-${id}.png`);
        await axe(["screenshot", "--udid", udid, "--output", join(runDir, shot)]);
      } catch {
        shot = ""; // a missing screenshot must never fail the step itself
      }

      emit({
        type: "step.finished", runId, udid, simName, stepId: id,
        status: "passed", durationMs: Date.now() - started, screenshot: shot,
      });

      if (replayOf) recorded.push({ ...replayOf(args), summary });
      return { content: [{ type: "text", text: result }] };
    } catch (err) {
      const detail = String(err?.message ?? err);
      emit({
        type: "step.finished", runId, udid, simName, stepId: id,
        status: "failed", durationMs: Date.now() - started, detail,
      });
      // Returned as content, not thrown: the agent should see the failure and
      // adapt (scroll, wait, try another selector) rather than abort the run.
      return { content: [{ type: "text", text: `FAILED: ${detail}` }], isError: true };
    }
  };
}

function simulatorTools(udid, runId, simName, runDir) {
  const wrap = (name, summarize, replayOf, fn) =>
    step(udid, runId, simName, runDir, name, summarize, replayOf, fn);

  return [
    tool(
      "describe_ui",
      "Read what is currently on screen. Returns the actionable elements with their labels, " +
        "identifiers, values and coordinates. Call this before acting, and again after acting " +
        "to confirm what changed.",
      {},
      wrap("describe_ui", () => "read screen", null, async () => {
        const screen = await describeUI(udid);
        if (!screen.elements?.length) {
          return "The screen has no actionable elements. It may still be loading.";
        }
        return JSON.stringify(screen.elements);
      }),
    ),

    tool(
      "tap",
      "Tap an element. Prefer `label` or `id` from describe_ui — selectors survive layout " +
        "changes, coordinates do not. Use x/y only when an element has neither.",
      {
        label: z.string().optional().describe("AXLabel of the element to tap"),
        id: z.string().optional().describe("accessibility identifier"),
        elementType: z.string().optional().describe("narrow an ambiguous match, e.g. Button"),
        x: z.number().optional(),
        y: z.number().optional(),
      },
      wrap(
        "tap",
        (a) => `tap ${a.label ? JSON.stringify(a.label) : a.id ? `#${a.id}` : `(${a.x},${a.y})`}`,
        (a) => ({ tool: "tap", label: a.label, id: a.id, x: a.x, y: a.y }),
        async (a) => {
          const args = ["tap", "--udid", udid];
          if (a.label) args.push("--label", a.label);
          else if (a.id) args.push("--id", a.id);
          else if (a.x != null && a.y != null) args.push("-x", String(a.x), "-y", String(a.y));
          else throw new Error("tap needs a label, id, or x/y");
          if (a.elementType) args.push("--element-type", a.elementType);
          // Poll rather than fail instantly: an element may still be animating in.
          if (a.label || a.id) args.push("--wait-timeout", "5");
          await axe(args);
          return "tapped";
        },
      ),
    ),

    tool(
      "type_text",
      "Type into the focused field. Tap the field first — this types wherever the keyboard " +
        "focus already is.",
      { text: z.string() },
      wrap(
        "type_text",
        (a) => `type ${JSON.stringify(a.text)}`,
        (a) => ({ tool: "type_text", text: a.text }),
        async (a) => {
          await axe(["type", a.text, "--udid", udid]);
          return "typed";
        },
      ),
    ),

    tool(
      "swipe",
      "Swipe between two points, to scroll or to page. Screen coordinates come from describe_ui.",
      {
        startX: z.number(), startY: z.number(),
        endX: z.number(), endY: z.number(),
        duration: z.number().optional().describe("seconds"),
      },
      wrap(
        "swipe",
        (a) => `swipe (${a.startX},${a.startY}) to (${a.endX},${a.endY})`,
        (a) => ({ tool: "swipe", startX: a.startX, startY: a.startY, endX: a.endX, endY: a.endY }),
        async (a) => {
          const args = ["swipe", "--udid", udid,
            "--start-x", String(a.startX), "--start-y", String(a.startY),
            "--end-x", String(a.endX), "--end-y", String(a.endY)];
          if (a.duration) args.push("--duration", String(a.duration));
          await axe(args);
          return "swiped";
        },
      ),
    ),

    tool(
      "press_button",
      "Press a hardware button: home, lock, side-button, siri, apple-pay.",
      { button: z.enum(["home", "lock", "side-button", "siri", "apple-pay"]) },
      wrap(
        "press_button",
        (a) => `press ${a.button}`,
        (a) => ({ tool: "press_button", button: a.button }),
        async (a) => {
          await axe(["button", a.button, "--udid", udid]);
          return "pressed";
        },
      ),
    ),
  ];
}

// ----------------------------------------------------------------------- main

function systemPrompt(simName) {
  return [
    "You are testing an iOS app on a simulator by driving its real UI.",
    `Your simulator is "${simName}". You can only reach this one.`,
    "",
    "How to work:",
    "- Call describe_ui first to see what is on screen. Never guess what is there.",
    "- Prefer tapping by label or id. Fall back to coordinates only when neither exists.",
    "- After acting, call describe_ui again to confirm what actually changed.",
    "- If an element is missing, it may need scrolling or more time. Try once or twice more,",
    "  then report honestly that you could not find it.",
    "",
    "Finishing: when the scenario is satisfied, or you are certain it cannot be, reply with",
    "exactly one line:",
    '  VERDICT: PASS <short reason>',
    '  VERDICT: FAIL <what you expected and what you saw instead>',
    "Report FAIL when the app genuinely misbehaves. Do not report PASS for something you did",
    "not actually verify on screen.",
  ].join("\n");
}

async function main() {
  const cfg = JSON.parse(process.argv[2] ?? "{}");
  const { udid, simName = udid, runId, scenario, runDir, maxTurns = 40, model } = cfg;

  if (!udid || !scenario) {
    process.stderr.write("agent: config needs at least udid and scenario\n");
    process.exit(2);
  }

  const server = createSdkMcpServer({
    name: "sim",
    tools: simulatorTools(udid, runId, simName, runDir),
  });

  let verdict = null;

  try {
    for await (const message of query({
      prompt: `Scenario to verify:\n\n${scenario}`,
      options: {
        model,
        maxTurns,
        systemPrompt: systemPrompt(simName),
        mcpServers: { sim: server },
        // The agent gets simulator tools and nothing else: no Bash, no file
        // access. It is here to drive a phone, not to touch the machine.
        allowedTools: ["mcp__sim__*"],
        permissionMode: "default",
      },
    })) {
      if (message.type === "assistant") {
        for (const block of message.message.content ?? []) {
          if (block.type === "text" && block.text.trim()) {
            emit({ type: "agent.thinking", runId, udid, simName, detail: block.text.trim() });
            const match = block.text.match(/VERDICT:\s*(PASS|FAIL)\s*(.*)/i);
            if (match) {
              verdict = {
                status: match[1].toUpperCase() === "PASS" ? "passed" : "failed",
                detail: match[2].trim(),
              };
            }
          }
        }
      }

      if (message.type === "result") {
        if (message.subtype !== "success" && !verdict) {
          verdict = { status: "error", detail: `agent ended: ${message.subtype}` };
        }
      }
    }
  } catch (err) {
    verdict = { status: "error", detail: String(err?.message ?? err) };
  }

  // An agent that stopped without stating a verdict has not verified anything,
  // so this is an error rather than a silent pass.
  if (!verdict) {
    verdict = { status: "error", detail: "agent finished without reporting a verdict" };
  }

  emit({ type: "agent.finished", runId, udid, simName, ...verdict });
  emit({ type: "replay", runId, udid, steps: recorded });
}

main().catch((err) => {
  process.stderr.write(`agent: ${err?.stack ?? err}\n`);
  process.exit(1);
});
