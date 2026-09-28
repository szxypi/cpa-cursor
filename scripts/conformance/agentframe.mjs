// Thin extraction of 9router's AgentService frame builder (executors/cursor.js
// module-private functions), copied line-for-line for wire conformance. Keep
// in sync with open-sse/executors/cursor.js when 9router changes.
import { encodeField, wrapConnectRPCFrame } from "./cursorProtobuf.js";

const PROTOBUF_LEN = 2;
const PROTOBUF_VARINT = 0;
const concatBuffers = (...chunks) => {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
};

const agentString = (field, value) => encodeField(field, PROTOBUF_LEN, value);
const agentMessage = (field, value) => encodeField(field, PROTOBUF_LEN, value);
const agentBool = (field, value) => encodeField(field, PROTOBUF_VARINT, value ? 1 : 0);

function textFromContent(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((part) => part?.type === "text" && typeof part.text === "string")
    .map((part) => part.text)
    .join("\n");
}

function encodeHistoryMessage(message) {
  const content = textFromContent(message?.content);
  if (!content) return null;

  const text = agentString(1, content);
  if (message.role === "assistant") {
    return agentMessage(2, agentMessage(1, agentMessage(1, text)));
  }
  return agentMessage(1, agentMessage(1, agentMessage(1, text)));
}

export function buildAgentRunFrame(messages, model) {
  const system = messages
    .filter((message) => message?.role === "system")
    .map((message) => textFromContent(message.content))
    .filter(Boolean)
    .join("\n\n");
  const chatMessages = messages.filter((message) => message?.role !== "system");
  const currentIndex = [...chatMessages].map((message) => message?.role).lastIndexOf("user");
  const current = currentIndex >= 0 ? chatMessages[currentIndex] : chatMessages.at(-1);
  const history = chatMessages
    .slice(0, currentIndex >= 0 ? currentIndex : -1)
    .map(encodeHistoryMessage)
    .filter(Boolean);
  const userText = textFromContent(current?.content) || "Continue.";

  const userMessage = concatBuffers(
    agentString(1, userText),
    agentString(2, crypto.randomUUID()),
  );
  const conversationHistory = history.length
    ? concatBuffers(...history.map((entry) => agentMessage(1, entry)))
    : null;
  const userAction = concatBuffers(
    agentMessage(1, userMessage),
    ...(conversationHistory ? [agentMessage(7, conversationHistory)] : []),
  );
  const conversationAction = agentMessage(1, userAction);
  const requestedModel = concatBuffers(agentString(1, model), agentBool(7, true));
  const runRequest = concatBuffers(
    agentMessage(1, new Uint8Array()),
    agentMessage(2, conversationAction),
    ...(system ? [agentString(8, system)] : []),
    agentMessage(9, requestedModel),
  );

  return wrapConnectRPCFrame(agentMessage(1, runRequest));
}
