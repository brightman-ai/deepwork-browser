"""Serial real Chrome baseline acceptance harness.
Usage: python3 tests/runtime-v2/baseline.py /absolute/dw-browser
No global binary install. Failure records survive. Uses actual native actions,
server persistence, explicit CSS value probe and exact expected-file bytes.
"""
from pathlib import Path
import hashlib, html, http.server, json, subprocess, sys, threading, time

ROOT = Path(__file__).resolve().parent / 'reports' / 'baseline'
TEXT = 'Updated 📌 note.\nHe said "ship" and wrote \'yes\'.\nReview on Monday.\n'
class Handler(http.server.BaseHTTPRequestHandler):
    saved = 'Original note.\n'
    broken = False
    def do_GET(self):
        page = '''<!doctype html><meta charset="utf-8"><title>Persisted note editor</title>
<style>body{font:20px system-ui;margin:40px}textarea{display:block;font:inherit;width:650px;height:180px}button,a{font:inherit;margin:20px}</style>
<label for="editor">Draft note</label><textarea id="editor">%s</textarea>
<button id="save">Save note</button><p id="status">Ready</p>
<script>document.getElementById('save').onclick=async()=>{const response=await fetch('/save',{method:'POST',body:document.getElementById('editor').value});if(response.ok)document.getElementById('status').textContent='Saved';};%s</script>''' % (html.escape(type(self).saved), "document.getElementById('editor').addEventListener('beforeinput',e=>e.preventDefault());" if type(self).broken else '')
        payload = page.encode()
        self.send_response(200); self.send_header('Content-Type','text/html; charset=utf-8'); self.send_header('Content-Length',str(len(payload))); self.end_headers(); self.wfile.write(payload)
    def do_POST(self):
        type(self).saved = self.rfile.read(int(self.headers['Content-Length'])).decode('utf-8')
        self.send_response(200); self.end_headers(); self.wfile.write(b'saved')
    def log_message(self,*_): pass

def main():
    binary = Path(sys.argv[1]).resolve()
    ROOT.mkdir(parents=True, exist_ok=True)
    expected = ROOT/'expected.txt'; expected.write_text(TEXT)
    wrong = ROOT/'wrong.txt'; wrong.write_text(TEXT+'unexpected extra line\n')
    server = http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    base = f'http://127.0.0.1:{server.server_port}'
    records = []
    try:
        for label, broken, checkfile, expected_rc in [('broken-input',True,expected,2),('restored-input',False,expected,0),('wrong-assertion',False,wrong,1),('restored-assertion',False,expected,0)]:
            Handler.broken=broken; Handler.saved='Original note.\n'
            spec={'version':2,'id':'final-'+label,'name':'Native edit save revisit exact bytes','environment':{'base_url':base,'entry_url':'/editor','mode':'headless','client':'chrome','clean_session':True,'viewport':'1280x800'},'journey':[
                {'id':'replace','kind':'action','action':{'kind':'fill','target':{'kind':'locator','locator':'css=#editor'}},'text_file':str(expected)},
                {'id':'save','kind':'action','action':{'kind':'click','target':{'kind':'locator','locator':'css=#save'}},'wait':{'assert':"text_contains('Saved')",'timeout_ms':5000}},
                {'id':'revisit','kind':'action','action':{'kind':'navigate','url':base+'/editor?revisit=1'}},
                {'id':'exact-value','kind':'probe','probes':[{'id':'editor','kind':'css','selector':'#editor'}],'check':[{'assert':f"probe_value_equals_file(id='editor',file='{checkfile}')"}]}
            ],'evidence':{'save':['observation','screenshot','trace','report','replay']}}
            path=ROOT/(label+'.journey.json'); path.write_text(json.dumps(spec,indent=2))
            argv=[str(binary),'journey','--file',str(path),'--scenario','app-test-baseline','--evidence',str(ROOT/(label+'-evidence')),'--fail-fast']
            started=time.monotonic(); proc=subprocess.run(argv,capture_output=True,text=True,timeout=180)
            (ROOT/(label+'.stdout.json')).write_text(proc.stdout); (ROOT/(label+'.stderr.log')).write_text(proc.stderr)
            records.append({'label':label,'argv':argv,'rc':proc.returncode,'expected_rc':expected_rc,'seconds':time.monotonic()-started,'server_saved_exact':Handler.saved==TEXT})
            (ROOT/'results.json').write_text(json.dumps(records,indent=2))
            print(json.dumps(records[-1]),flush=True)
            if proc.returncode != expected_rc: raise RuntimeError(f'{label}: expected {expected_rc}, got {proc.returncode}')
            if not broken and Handler.saved!=TEXT: raise RuntimeError(label+': independent server persistence did not match exact UTF-8 bytes')
    finally:
        server.shutdown(); server.server_close()
        (ROOT/'candidate.json').write_text(json.dumps({'binary':str(binary),'sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'expected_text_sha256':hashlib.sha256(expected.read_bytes()).hexdigest(),'timestamp':time.time(),'records':records},indent=2))
if __name__=='__main__': main()
