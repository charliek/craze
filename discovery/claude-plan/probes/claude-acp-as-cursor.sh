#!/bin/sh
# CL0 probe 2b: lets unmodified craze run claude-agent-acp in cursor-agent's
# place. It ignores the cursor arguments craze passes (--trust acp --force) and
# starts the adapter with a clean environment and the user's own `claude`.
# ADAPTER_JS: path to the adapter's dist/index.js (see README.md).
: "${ADAPTER_JS:?set ADAPTER_JS to claude-agent-acp's dist/index.js}"
exec env -i HOME="$HOME" USER="$USER" LANG="${LANG:-C.UTF-8}" PATH="$PATH" \
  CLAUDE_CODE_EXECUTABLE="${CLAUDE_CODE_EXECUTABLE:-$HOME/.local/bin/claude}" \
  CLAUDE_AGENT_ACP_PROTOCOL=v1 \
  node "$ADAPTER_JS"
