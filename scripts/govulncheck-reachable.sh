#!/usr/bin/env bash
set -euo pipefail

TMP_JSON="$(mktemp)"
trap 'rm -f "$TMP_JSON"' EXIT

./.bin/govulncheck -format json ./... > "$TMP_JSON"

python3 - "$TMP_JSON" <<'PY2'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
decoder = json.JSONDecoder()
pos = 0
reachable = []
module_only = []
while True:
    while pos < len(text) and text[pos].isspace():
        pos += 1
    if pos >= len(text):
        break
    obj, pos = decoder.raw_decode(text, pos)
    finding = obj.get('finding') if isinstance(obj, dict) else None
    if not finding:
        continue
    trace = finding.get('trace') or []
    osv = finding.get('osv', 'unknown')
    if len(trace) == 1 and set(trace[0].keys()) <= {'module', 'version'}:
        module_only.append((osv, trace[0].get('module', 'unknown'), trace[0].get('version', 'unknown')))
        continue
    reachable.append((osv, trace))

if module_only:
    print('Ignoring module-only vulnerabilities:')
    for osv, module, version in module_only:
        print(f'  - {osv} {module}@{version}')

if reachable:
    print('Reachable vulnerabilities found:', file=sys.stderr)
    for osv, trace in reachable:
        print(f'  - {osv}', file=sys.stderr)
    sys.exit(1)

print('No reachable vulnerabilities found.')
PY2
