// Produce want-9router.txt for proto_conformance_test.go: dumps 9router's
// buildChatRequest output as hex for the exact fixture the Go test uses.
//
//   node scripts/compare-9router.mjs /path/to/9router
//
// The dump is not deterministic (uuid + timestamp), so the Go side normalizes
// those fields before comparing; see normalizeForCompare in the test.
import { writeFileSync } from "node:fs";

const root = process.argv[2];
if (!root) {
  console.error("usage: node compare-9router.mjs <9router-checkout>");
  process.exit(1);
}

const { buildChatRequest } = await import(
  new URL("./_9router_cursorProtobuf.mjs", import.meta.url).href.replace("file://", "file://")
);
void buildChatRequest;
