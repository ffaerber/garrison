# shellcheck shell=bash
# threads: who answers in a thread of todo's channel: a plain message goes
# to no agent, the Reply button goes on with the agent, and another agent
# mentioned answers there too (test/ui/threads.py). From the fixture team.
section_threads() {
  [ -s "$ETC/llm-api-key" ] || { echo "skip  no LLM key in $ETC/llm-api-key"; return 0; }
  ui_run threads
}
