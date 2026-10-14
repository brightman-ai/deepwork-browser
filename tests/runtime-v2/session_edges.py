"""Real CLI wait/debug/public-origin checks; run serially with Chrome available."""
from pathlib import Path
import argparse
import functools
import hashlib
import http.server
import json
import subprocess
import threading
import time

parser = argparse.ArgumentParser()
parser.add_argument('--binary', required=True)
parser.add_argument('--evidence', required=True)
args = parser.parse_args()
binary = Path(args.binary).resolve()
out = Path(args.evidence).resolve()
out.mkdir(parents=True, exist_ok=False)
records = []
handler = functools.partial(http.server.SimpleHTTPRequestHandler,
                            directory=str(Path(__file__).parent / 'fixtures'))
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
sid = 'session-edges-' + str(time.time_ns())
public_sid = sid + '-public'
owned = []

def call(label, *argv, expected=0):
    p = subprocess.run([str(binary), *argv], capture_output=True, text=True, timeout=150)
    payload = json.loads(p.stdout)
    records.append({'label': label, 'argv': argv, 'rc': p.returncode,
                    'stdout': payload, 'stderr': p.stderr})
    (out / 'results.json').write_text(json.dumps(records, indent=2))
    print(label, p.returncode, flush=True)
    assert p.returncode == expected, (label, p.returncode, payload)
    return payload

try:
    url = f'http://127.0.0.1:{server.server_port}/note-editor.html'
    call('open', 'open', url, '--id', sid, '--scenario', 'app-test-explore',
         '--mode', 'headless', '--evidence', str(out / 'observations'))
    owned.append(sid)
    before = call('before-debug', 'observe', '--id', sid)
    old = next(e['ref'] for e in before['elements'] if e['role'] == 'textbox')
    result = call('debug-read', 'eval', 'document.title', '--id', sid)
    assert result['intervention'] and result['observation_invalidated']
    stale = call('debug-old-ref', 'act', 'fill', old, '--id', sid,
                 '--text', 'must not replace', expected=2)
    assert stale['outcome']['dispatch_state'] == 'not-dispatched'
    fresh = call('after-debug', 'observe', '--id', sid)
    value = next(e['value'] for e in fresh['elements'] if e['role'] == 'textbox')
    assert value.startswith('Original'), value
    call('wait-duration', 'wait', '--id', sid, '--duration', '50ms')
    call('wait-condition', 'wait', '--id', sid, '--assert',
         "url_matches('/note-editor.html')", '--timeout', '5s')
    missed = call('wait-missing-condition', 'wait', '--id', sid, '--assert',
                  "url_matches('/never-present')", '--timeout', '500ms', expected=2)
    assert missed['status'] == 'BLOCKED'
    call('close-local', 'close', '--id', sid)
    owned.remove(sid)
    call('public-open', 'open', 'https://example.com', '--id', public_sid,
         '--scenario', 'webvisit', '--mode', 'headless',
         '--evidence', str(out / 'public-observations'))
    owned.append(public_sid)
    public = call('public-observe', 'observe', '--id', public_sid)
    assert public['url'].startswith('https://example.com'), public
    denied = call('public-debug-denied', 'eval', '1', '--id', public_sid, expected=2)
    assert 'allow-host' in json.dumps(denied) or 'origin' in json.dumps(denied)
    call('close-public', 'close', '--id', public_sid)
    owned.remove(public_sid)
    (out / 'success.json').write_text(json.dumps({
        'verified': True, 'sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
        'checks': len(records)}))
finally:
    for session in owned:
        call('cleanup-' + session, 'close', '--id', session)
    server.shutdown()
    server.server_close()
