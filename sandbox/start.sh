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
# Git: the platform's repos through the agent service, read-only, as the
# person whose turn runs (see homecloud.ts for proposing changes).
export GIT_CONFIG_COUNT=4
export GIT_CONFIG_KEY_0="http.$HOMECLOUD_API/git/.extraHeader" GIT_CONFIG_VALUE_0="Authorization: Bearer $HOMECLOUD_TOKEN"
export GIT_CONFIG_KEY_1=user.name GIT_CONFIG_VALUE_1="Agent"
export GIT_CONFIG_KEY_2=user.email GIT_CONFIG_VALUE_2="agent@sandbox"
export GIT_CONFIG_KEY_3=advice.detachedHead GIT_CONFIG_VALUE_3=false
export HOMECLOUD_GIT="$HOMECLOUD_API/git"
cd /data/work
exec pi --mode rpc --offline --provider homecloud --model "$HOMECLOUD_MODEL" \
  --system-prompt "$HOMECLOUD_PROMPT" --session-dir /data/sessions --continue \
  --no-extensions --extension /opt/homecloud/homecloud.ts --no-skills --no-prompt-templates
