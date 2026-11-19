# shellcheck shell=bash
# guided: vague requests in todo's channel, and the agents asking what is
# meant (test/ui/guided.py). From the fixture team.
section_guided() {
  [ -s "$ETC/llm-api-key" ] || { echo "skip  no LLM key in $ETC/llm-api-key"; return 0; }
  ui_run guided
}
