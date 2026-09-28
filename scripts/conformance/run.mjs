// Fixture encoder for proto_conformance_test.go. The sandbox pins uuid + Date
// so the dump is byte-stable; the Go test injects the same values.
import { writeFileSync } from "node:fs";

const RealDate = Date;
globalThis.Date = class extends RealDate {
  constructor(...args) {
    if (args.length === 0) {
      super("2026-09-28T12:00:00.000Z");
    } else {
      super(...args);
    }
  }
  static now() {
    return RealDate.parse("2026-09-28T12:00:00.000Z");
  }
};

const { buildChatRequest } = await import("./cursorProtobuf.js");
const { buildAgentRunFrame } = await import("./agentframe.mjs");

// Deterministic crypto.randomUUID for the agent frame dump.
let agentCounter = 0;
Object.defineProperty(globalThis, "crypto", {
  configurable: true,
  value: {
    randomUUID() {
      agentCounter += 1;
      const tail = String(agentCounter).padStart(4, "0");
      return `10000000-0000-4000-8000-00000000${tail}`;
    },
  },
});

const messages = [
  { role: "user", content: "hello" },
  { role: "assistant", content: "hi there" },
  { role: "user", content: "and <tool_result>\n<tool_name>t</tool_name>\n</tool_result> done" },
];
const tools = [
  {
    type: "function",
    function: {
      name: "get_weather",
      description: "Get weather",
      parameters: {
        type: "object",
        properties: { city: { type: "string" } },
        required: ["city"],
      },
    },
  },
  {
    type: "function",
    function: {
      name: "mcp__server__read_file",
      description: "Reads a file",
      parameters: { type: "object", properties: { path: { type: "string" } } },
    },
  },
];

const body = buildChatRequest(messages, "claude-4.5-sonnet", tools, "medium", true);

const out = new URL("../../want-9router.txt", import.meta.url);
writeFileSync(out, Buffer.from(body).toString("hex") + "\n");

const agentFixture = [
  { role: "system", content: "be terse" },
  { role: "user", content: "hi" },
  { role: "assistant", content: "hello" },
  { role: "user", content: "what is 2+2" },
];
const agentFrame = buildAgentRunFrame(agentFixture, "composer-1");

const agentOut = new URL("../../want-9router-agent.txt", import.meta.url);
writeFileSync(agentOut, Buffer.from(agentFrame).toString("hex") + "\n");
console.log(`wrote ${out.pathname} (${body.length} B) + ${agentOut.pathname} (${agentFrame.length} B)`);
