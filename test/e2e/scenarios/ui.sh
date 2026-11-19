# shellcheck shell=bash
# ui: the same, and more, clicked and typed in the browser (test/ui).
section_ui() {
  echo "# the browser scenarios (test/ui), in Playwright's Chromium"
  test/ui/run.sh 2>"$tmp/ui.err" | tee "$tmp/ui.out" || true
  local n
  n=$(grep -c '^FAIL' "$tmp/ui.out" || true)
  # done() prints one of these last; without it the script broke off.
  grep -Eq '^(all checks passed|[0-9]+ check\(s\) failed)' "$tmp/ui.out" ||
    { echo "FAIL  the browser scenarios ran to the end"; tail -15 "$tmp/ui.err"; n=$((n + 1)); }
  failed=$((failed + n))
}
