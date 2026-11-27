#!/usr/bin/env bash
# test/vm/ask-bank.sh VM [QUESTIONS]: asks the agents in VM the questions of
# test/e2e/questions.tsv (or QUESTIONS), each in a fresh private chat, and
# fetches the answers to bin/ask-bank/<time>.jsonl, to read: how the agents
# answer what people really ask. VM is one from the fixture team or after it,
# e.g. garrison-k3s-commander (KEEP=1 test/vm/steps.sh commander).
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
VM=${1:?usage: test/vm/ask-bank.sh VM [QUESTIONS]}
bank=${2:-$ROOT/test/e2e/questions.tsv}
cd "$ROOT"
mkdir -p bin/ask-bank
stamp=$(date +%Y%m%d-%H%M%S)
multipass transfer "$bank" "$VM:questions.tsv"
multipass transfer test/e2e/ask-bank.sh "$VM:ask-bank.sh"
multipass transfer test/e2e/lib.sh "$VM:lib.sh"
multipass exec "$VM" -- sudo bash -c 'install -m 755 ~ubuntu/ask-bank.sh ~ubuntu/garrison/test/e2e/ask-bank.sh && install -m 644 ~ubuntu/lib.sh ~ubuntu/garrison/test/e2e/lib.sh &&
  cd ~ubuntu/garrison && test/e2e/ask-bank.sh ~ubuntu/questions.tsv /var/tmp/ask-bank.jsonl && chmod 644 /var/tmp/ask-bank.jsonl'
multipass transfer "$VM:/var/tmp/ask-bank.jsonl" "bin/ask-bank/$stamp.jsonl"
echo "answers: bin/ask-bank/$stamp.jsonl"
