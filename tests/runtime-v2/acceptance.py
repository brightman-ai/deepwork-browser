"""Real CLI system acceptance. No DOM preparation or browser mocks."""
from pathlib import Path
import argparse, hashlib, http.server, json, subprocess, threading, time

ROOT = Path(__file__).resolve().parent
parser = argparse.ArgumentParser()
parser.add_argument('--binary', required=True)
parser.add_argument('--label', required=True)
parser.add_argument('--evidence-root', default=str(ROOT/'reports'))
opt = parser.parse_args()
CLI = str(Path(opt.binary).resolve())
OUT = Path(opt.evidence_root).resolve() / opt.label
OUT.mkdir(parents=True, exist_ok=False)
SID = 'parent-v2-' + str(time.time_ns())
records, submissions, issues = [], [], []
normal = (ROOT / 'fixtures/note-editor.html').read_text()
canvas = '''<!doctype html><meta charset="utf-8"><title>Native canvas controls</title>
<style>body{margin:40px;font:20px system-ui}canvas{display:block;width:600px;height:200px;background:#def}</style><h1>Native canvas controls</h1><canvas width="600" height="200"></canvas><pre id="events">Ready</pre><script>
const c=document.querySelector('canvas');let start=null;const events=[];
function record(event,extra={}){const fact={kind:'canvas',event,trusted:true,...extra};events.push(fact);document.getElementById('events').textContent=JSON.stringify(events);fetch('/receipt',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(fact)});}
c.addEventListener('dblclick',e=>record('double',{trusted:e.isTrusted}));
c.addEventListener('contextmenu',e=>{e.preventDefault();record('right',{trusted:e.isTrusted});});
c.addEventListener('pointerdown',e=>{start=[e.clientX,e.clientY];});
c.addEventListener('pointerup',e=>{if(start&&Math.abs(e.clientX-start[0])>50)record('drag',{trusted:e.isTrusted});start=null;});
c.addEventListener('wheel',e=>{e.preventDefault();record('wheel',{trusted:e.isTrusted,delta_y:e.deltaY});},{passive:false});
c.addEventListener('touchend',e=>record('touch',{trusted:e.isTrusted}));
</script>'''
text = 'Updated 📌 note.\nHe said "ship" and wrote \'yes\'.\nReview on Monday.\n'
(OUT / 'text.txt').write_text(text)
initial = 'original unchanged'

def editor(kind):
    attributes = {
        'key': 'onkeydown="if(event.key===\'a\')event.preventDefault()"',
        'delete': 'onbeforeinput="if(event.inputType===\'deleteContentBackward\')event.preventDefault()"',
        'insert': 'onbeforeinput="if(event.inputType===\'insertText\')event.preventDefault()"',
        'readonly': 'readonly', 'disabled': 'disabled',
    }.get(kind, '')
    field = f'<label for="draft">Draft note</label><textarea id="draft" {attributes}>{initial}</textarea>'
    script = "const draft=document.getElementById('draft');"
    if kind == 'password':
        field = '<label for="draft">Draft note</label><input id="draft" type="password" value="old-masked-value">'
    if kind == 'shadow':
        field = '<div id="host"></div>'
        script = "const shadow=document.getElementById('host').attachShadow({mode:'open'});shadow.innerHTML=`<style>textarea{font:20px system-ui;width:500px;height:100px}</style><label for=\"draft\">Draft note</label><textarea id=\"draft\">original unchanged</textarea>`;const draft=shadow.querySelector('textarea');"
    if kind == 'controlled':
        script += "let model=draft.value;draft.addEventListener('input',()=>{model=draft.value;queueMicrotask(()=>{draft.value=model;});});"
    script += "document.getElementById('save').onclick=()=>{const text=draft.value;document.getElementById('saved').textContent=text;fetch('/receipt',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({kind:" + json.dumps(kind) + ",text})});};"
    return '<!doctype html><meta charset="utf-8"><title>Editor '+kind+'</title><style>body{font:20px system-ui;margin:40px}label,textarea{display:block}textarea{font:inherit;width:500px;height:100px}button{font:inherit;margin:20px}pre{white-space:pre-wrap}</style><h1>Editor '+kind+'</h1>'+field+'<button id="save">Save note</button><pre id="saved"></pre><script>'+script+'</script>'

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        kind = self.path.strip('/').split('?')[0]
        payload = canvas if kind=='canvas' else normal if kind in ('', 'notes') else editor(kind)
        data = payload.encode()
        self.send_response(200); self.send_header('Content-Type','text/html; charset=utf-8'); self.end_headers(); self.wfile.write(data)
    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        submissions.append(data)
        self.send_response(204); self.end_headers()
    def log_message(self, *_): pass

server = http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
threading.Thread(target=server.serve_forever,daemon=True).start()
URL = f'http://127.0.0.1:{server.server_port}'

def call(label,*args,expected=0,timeout=60):
    started=time.monotonic()
    proc=subprocess.run([CLI,*args],text=True,capture_output=True,timeout=timeout)
    record={'label':label,'argv':list(args),'rc':proc.returncode,'seconds':round(time.monotonic()-started,3),'stdout':proc.stdout,'stderr':proc.stderr}
    records.append(record); (OUT/'results.json').write_text(json.dumps(records,indent=2))
    (OUT/(label+'.stdout.json')).write_text(proc.stdout); (OUT/(label+'.stderr.log')).write_text(proc.stderr)
    print(json.dumps({'label':label,'rc':proc.returncode,'seconds':record['seconds']}),flush=True)
    allowed = (expected,) if isinstance(expected,int) else expected
    if proc.returncode not in allowed: raise RuntimeError(f'{label}: rc={proc.returncode}, expected {allowed}: {proc.stdout} {proc.stderr[-1000:]}')
    return json.loads(proc.stdout)

def observe(label): return call(label,'observe','--id',SID,'--output',str(OUT/(label+'.reply.json')))
def target(data,role,name):
    for element in data.get('elements',[]):
        if element.get('role')==role and element.get('name')==name: return element['ref']
    raise RuntimeError(f'missing visible {role} {name}: {data}')
def act(label,verb,*args,expected=0): return call(label,'act',verb,*args,'--id',SID,expected=expected)
def reject(label,*args):
    result=call(label,*args,expected=2)
    if result.get('dispatch_state',result.get('outcome',{}).get('dispatch_state'))!='not-dispatched':
        issues.append({'label':label,'failure':'missing truthful not-dispatched outcome','actual':result})
        (OUT/'issues.json').write_text(json.dumps(issues,indent=2))
def save_receipt(label,want,kind):
    data=observe(label+'-ready')
    act(label+'-save','click',target(data,'button','Save note'))
    observed=observe(label+'-saved')
    assert submissions and submissions[-1]=={'kind':kind,'text':want},submissions
    return observed

opened=False
try:
    (OUT/'build.json').write_text(json.dumps({'binary':CLI,'sha256':hashlib.sha256(Path(CLI).read_bytes()).hexdigest(),'scope':'independent real coordinator CLI acceptance; source may still be WIP'},indent=2))
    call('open','open',URL+'/notes','--id',SID,'--scenario','app-test-explore','--mode','headless','--persona','desktop','--viewport','1280x800','--evidence',str(OUT/'observations'),timeout=150); opened=True
    reject('duplicate-open','open',URL,'--id',SID,'--scenario','app-test-explore')
    reject('missing-id','observe')
    reject('observe-environment','observe','--id',SID,'--viewport','880x720')
    before=observe('notes-before')
    old=target(before,'textbox','Draft note')
    reject('missing-text','act','fill',old,'--id',SID)
    act('notes-fill','fill',old,'--text-file',str(OUT/'text.txt'))
    data=observe('notes-after-fill')
    act('notes-save','click',target(data,'button','Save note'))
    data=observe('notes-after-save')
    act('notes-open-saved','click',target(data,'link','Open saved note'))
    data=observe('notes-saved-view')
    call('notes-check','check','--id',SID,'--assert',"text_contains('Review on Monday.')")
    act('notes-reload','reload')
    observe('notes-reloaded')
    call('notes-reload-check','check','--id',SID,'--assert',"text_contains('Review on Monday.')")
    act('notes-return-edit','click',target(observe('notes-return-ready'),'link','Return to editing'))
    data=observe('wide-editor'); old=target(data,'textbox','Draft note')
    call('resize','session','viewport','--id',SID,'--size','880x720')
    reject('resize-old-ref','act','fill',old,'--id',SID,'--text','must not replace')
    data=observe('narrow-editor')
    call('narrow-status','session','status','--id',SID)
    assert Path(data['screenshot']).is_file()
    act('narrow-focus','focus',target(data,'textbox','Draft note'))
    observe('focused-proof-observe')
    act('focused-insert','type','--focused','--text',' Additional focused text.')
    data=observe('focused-after')
    old=target(data,'textbox','Draft note')
    call('takeover','htr','takeover','--id',SID)
    reject('takeover-block','act','fill',old,'--id',SID,'--text','must not replace')
    call('takeover-status','htr','status','--id',SID)
    call('yield','htr','yield','--id',SID)
    reject('yield-old-ref','act','fill',old,'--id',SID,'--text','must not replace')
    data=observe('yield-fresh')
    act('yield-fill','fill',target(data,'textbox','Draft note'),'--text-file',str(OUT/'text.txt'))
    original_tabs=call('tabs-initial','tabs','list','--id',SID)
    original_view=observe('tabs-original-ready'); original_ref=target(original_view,'textbox','Draft note')
    created=call('tabs-new','tabs','new','--id',SID,'--url',URL+'/controlled')
    reject('tabs-old-ref','act','fill',original_ref,'--id',SID,'--text','must not replace')
    observe('tabs-new-view')
    tabs=call('tabs-after-new','tabs','list','--id',SID)
    assert len(tabs['targets'])==len(original_tabs['targets'])+1
    new_id=created.get('created_target_id',created.get('target_id'))
    original_id=original_tabs['current_target_id']
    call('tabs-select-original','tabs','select','--id',SID,'--target',original_id)
    observe('tabs-original-view')
    call('tabs-close-new','tabs','close','--id',SID,'--target',new_id)
    tabs=call('tabs-final','tabs','list','--id',SID)
    assert len(tabs['targets'])==len(original_tabs['targets'])
    for kind in ('key','delete','insert','readonly','disabled','controlled','shadow'):
        act(kind+'-navigate','navigate','--url',URL+'/'+kind)
        data=observe(kind+'-before')
        if kind=='disabled':
            ref='css=#draft'
        else: ref=target(data,'textbox','Draft note')
        success=kind in ('controlled','shadow')
        act(kind+'-fill','fill',ref,'--text-file',str(OUT/'text.txt'),expected=0 if success else (1,2))
        save_receipt(kind,text if success else '' if kind=='insert' else initial,kind)
    # Observation tokens must not alias even without an intervening action.
    one=observe('identity-one'); two=observe('identity-two')
    assert target(one,'textbox','Draft note')!=target(two,'textbox','Draft note')
    reject('identity-old-ref','act','fill',target(one,'textbox','Draft note'),'--id',SID,'--text','must not replace')
    # Canonical files and referenced images are required, even for URL checks.
    reply=OUT/'identity-two.reply.json'
    call('artifact-pass','check','--observation',str(reply),'--assert',"url_matches('/shadow')")
    image=Path(two['screenshot']); original=image.read_bytes(); image.unlink()
    call('artifact-image-missing','check','--observation',str(reply),'--assert',"url_matches('/shadow')",expected=2)
    image.write_bytes(original)
    call('artifact-image-restored','check','--observation',str(reply),'--assert',"url_matches('/shadow')")
    (OUT/'empty.json').write_text('{}')
    call('artifact-empty','check','--observation',str(OUT/'empty.json'),'--assert','console_errors_count == 0',expected=2)
    call('artifact-telemetry-unavailable','check','--observation',str(reply),'--assert','console_errors_count == 0',expected=2)
    act('canvas-navigate','navigate','--url',URL+'/canvas')
    for label,verb,extra,event in (
        ('canvas-double','click',('--count','2'),'double'),
        ('canvas-right','click',('--button','right'),'right'),
        ('canvas-drag','drag',('--end-x','300','--end-y','160'),'drag'),
        ('canvas-wheel','scroll',('--direction','down'),'wheel'),
    ):
        first=observe(label+'-first'); current=observe(label+'-current')
        rejects_before=len(submissions)
        reject(label+'-stale','act',verb,'--id',SID,'--x','100','--y','160','--unit','css-px','--frame','viewport','--observation-id',first['observation_id'],*extra)
        assert len(submissions)==rejects_before
        act(label,verb,'--x','100','--y','160','--unit','css-px','--frame','viewport','--observation-id',current['observation_id'],*extra)
        observe(label+'-after')
        assert submissions[-1]['kind']=='canvas' and submissions[-1]['event']==event and submissions[-1]['trusted'],submissions[-1]
    # Sensitive unmasked editors reject before deletion; masked inputs retain privacy.
    act('sensitive-navigate','navigate','--url',URL+'/sensitive')
    sensitive_before=observe('sensitive-before')
    secret='private-test-sentinel-'+str(time.time_ns())
    secret_file=OUT/'sensitive-text.txt'; secret_file.write_text(secret)
    response=act('sensitive-unmasked-reject','fill',target(sensitive_before,'textbox','Draft note'),'--sensitive','--text-file',str(secret_file),expected=2)
    assert response.get('outcome',{}).get('dispatch_state')=='not-dispatched'
    assert secret not in json.dumps(response)
    save_receipt('sensitive-unmasked',initial,'sensitive')
    act('password-navigate','navigate','--url',URL+'/password')
    sensitive_before=observe('password-before')
    call('record-start','record','start','--id',SID)
    response=act('password-fill','fill',target(sensitive_before,'textbox','Draft note'),'--sensitive','--text-file',str(secret_file))
    assert secret not in json.dumps(response)
    sensitive_after=observe('sensitive-after')
    assert secret not in json.dumps(sensitive_after)
    canonical=Path(sensitive_after['observation_path'])
    assert secret not in canonical.read_text()
    trace=call('record-stop','record','stop','--id',SID)
    assert secret not in json.dumps(trace)
    if issues: raise RuntimeError('Required contract checks failed: '+json.dumps(issues))
    (OUT/'success.json').write_text(json.dumps({'verified':True,'checks':len(records),'binary':CLI}))
finally:
    if opened:
        try: call('close','close','--id',SID)
        except Exception as error: print('OWNED CLOSE FAILED:',error,flush=True)
    server.shutdown(); server.server_close()
    (OUT/'receipts.json').write_text(json.dumps(submissions,indent=2))
    print('Owned local server closed.',flush=True)
