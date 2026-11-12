#!/bin/sh
# The sandbox's main process: Pi in RPC mode, driven by the agent service over
# stdin and stdout. Set by the agent service:
#   HOMECLOUD_API     the agent service's address for sandboxes (LLM and tools)
#   HOMECLOUD_TOKEN   names this chat to it
#   HOMECLOUD_MODEL   the agent's model
#   HOMECLOUD_PROMPT  the agent's system prompt with the platform's rules
# /data is the chat's volume: the workspace and Pi's session, kept between
# containers, so a chat resumes where it left off.
set -eu
mkdir -p /data/work /data/sessions "$HOME/.pi/agent"
# The LLM through the agent service, which adds the key; the token is read
# from the environment by name.
cat > "$HOME/.pi/agent/models.json" <<EOF
{"providers": {"homecloud": {
  "baseUrl": "$HOMECLOUD_API/v1", "api": "openai-completions", "apiKey": "HOMECLOUD_TOKEN",
  "compat": {"supportsDeveloperRole": false},
  "models": [{"id": "$HOMECLOUD_MODEL", "reasoning": true, "contextWindow": 128000}]}}}
EOF
cd /data/work
exec pi --mode rpc --offline --provider homecloud --model "$HOMECLOUD_MODEL" \
  --system-prompt "$HOMECLOUD_PROMPT" --session-dir /data/sessions --continue \
  --no-extensions --extension /opt/homecloud/homecloud.ts --no-skills --no-prompt-templates
