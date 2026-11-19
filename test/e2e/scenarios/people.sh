# shellcheck shell=bash
# people: joining and leaving todo, as each person sees it in Open WebUI
# (test/ui/people.py). From the fixture deploy: alice is todo's only person.
section_people() {
  [ -s "$ETC/llm-api-key" ] || { echo "skip  no LLM key in $ETC/llm-api-key"; return 0; }
  ui_run people
}
