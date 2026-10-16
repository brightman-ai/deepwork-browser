"""Serial CLI regression for exact native select and cross-process privacy.
Usage: python3 tests/runtime-v2/select_exact.py /absolute/dw-browser [privacy-only]
Only owned headless sessions and a loopback fixture are created. No installation.
"""
from pathlib import Path
import subprocess, json, time, http.server, threading, sys, hashlib
from html import escape

binary = str(Path(sys.argv[1]).resolve())
privacy_only = len(sys.argv)>2 and sys.argv[2]=='privacy-only'
root = Path(__file__).resolve().parent/'reports'/'select-exact'/str(time.time_ns())
fingerprint = hashlib.sha256(Path(binary).read_bytes()).hexdigest()
root.mkdir(parents=True, exist_ok=True)
sid = 'select-exact-parent-'+str(time.time_ns())
records = []
events = {}

class Fixture(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        from urllib.parse import urlsplit,parse_qs
        u=urlsplit(self.path)
        q=parse_qs(u.query)
        if u.path=='/favicon.ico':
            self.send_response(404);self.end_headers();return
        if u.path=='/event':
            mode=q['mode'][0]
            events.setdefault(mode,[]).append(q)
            self.send_response(204); self.end_headers(); return
        mode=u.path.strip('/') or 'collision'
        choices={
          'collision':[('Clash','By value'),('second','Clash'),('initial','Initial')],
          'dup-label':[('a','Duplicate'),('b','Duplicate'),('initial','Initial')],
          'dup-value':[('same','Alpha'),('same','Beta'),('initial','Initial')],
          'identical':[('same','Duplicate'),('same','Duplicate')],
          'empty':[('a','Alpha'),('','全部')],
          'large':[(str(i),'Choice '+str(i)) for i in range(25)],
          'privacy':[('a','Alpha'),('full','全文识别')],
        }[mode]
        if mode=='large': choices[22]=('22','Choice 0')
        selected=(0 if mode in ('identical','empty','privacy') else len(choices)-1)
        opts=''.join('<option value="'+escape(v,quote=True)+'"'+(' selected' if i==selected else '')+'>'+escape(l)+'</option>' for i,(v,l) in enumerate(choices))
        password='<label>Password<input aria-label="Password" type="password"></label>' if mode=='privacy' else ''
        data=('''<!doctype html><meta charset="utf-8"><title>Exact choice task</title>'''+password+'''<label>Source<select aria-label="Source">'''+opts+'''</select></label><p id="feedback">initial</p><button onclick="document.getElementById('feedback').textContent='continued'">Continue</button><script>
 const select=document.querySelector('select');
 for(const type of ['input','change'])select.addEventListener(type,e=>{
 const s=select.selectedIndex;document.getElementById('feedback').textContent='selected '+s+' '+select.value+' trusted='+e.isTrusted;
 fetch('/event?mode='''+mode+'''&type='+type+'&index='+s+'&trusted='+e.isTrusted);
 });</script>''').encode()
        self.send_response(200);self.send_header('Content-Type','text/html; charset=utf-8');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    def log_message(self,*args):pass

def call(label,args,want=0):
    p=subprocess.run([binary,*args],capture_output=True,text=True,timeout=40)
    (root/(label+'.json')).write_text(p.stdout)
    (root/(label+'.stderr')).write_text(p.stderr)
    d=json.loads(p.stdout)
    records.append({'case':label,'exit':p.returncode,'execution':d.get('execution'),'matched_by':d.get('selection',{}).get('matched_by')})
    (root/'results.json').write_text(json.dumps(records,ensure_ascii=False,indent=2))
    assert p.returncode==want,(label,d)
    print(label,p.returncode,d.get('execution'),flush=True)
    return d

def obs(label):return call(label,['observe','--id',sid,'--content','text','--top','50'])
def control(d,name='Source'):return next(e for e in d['elements'] if e.get('name')==name)
def page(mode):
    call(mode+'-navigate',['act','navigate','--id',sid,'--url',url+'/'+mode])
    return obs(mode+'-observe')
def select(label,d,field,text,want=0):
    a=call(label,['act','select',control(d)['ref'],'--id',sid,'--'+field,text],want)
    if want==0:
        assert a['execution']=='native-keys' and a['selection']['verified'] and a['selection']['matched_by']==field,a
    else:assert a.get('outcome',a).get('dispatch_state')=='not-dispatched',a
    return a

server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Fixture)
threading.Thread(target=server.serve_forever,daemon=True).start()
url='http://127.0.0.1:'+str(server.server_port)
opened=False
try:
    call('open',['open',url+'/collision','--id',sid,'--scenario','app-test-explore','--mode','headless','--persona','desktop','--evidence',str(root)])
    opened=True
    if not privacy_only:
        d=obs('collision-before')
        a=select('collision-label',d,'label','Clash');assert a['selection']['after']['index']==1
        d=obs('collision-after-label');assert 'selected 1 second trusted=true' in d['content']['text']
        a=select('collision-value',d,'value','Clash');assert a['selection']['after']['index']==0
        d=obs('collision-after-value');assert 'selected 0 Clash trusted=true' in d['content']['text']
        before=len(events.get('collision',[]))
        select('prefix-refused',d,'label','Cla',2)
        for label,extra in [('both-refused',['--label','Clash','--value','second']),('missing-refused',[]),('duplicate-flag-refused',['--label','Clash','--label','Initial'])]:
            a=call(label,['act','select',control(d)['ref'],'--id',sid,*extra],2)
            assert a.get('outcome',a).get('dispatch_state')=='not-dispatched'
        assert len(events.get('collision',[]))==before
        for mode,ambiguous_field,ambiguous in [('dup-label','label','Duplicate'),('dup-value','value','same'),('identical','label','Duplicate')]:
            d=page(mode);before=len(events.get(mode,[]))
            select(mode+'-refused',d,ambiguous_field,ambiguous,2)
            assert len(events.get(mode,[]))==before
            if mode=='identical':
                assert all(not o['selectable'] and 'option' not in o for o in control(d)['select']['options'])
                select('identical-value-refused',d,'value','same',2)
            else:
                o=control(d)['select']['options'][0]
                assert len(o['option'])==1
                field,text=next(iter(o['option'].items()))
                assert field!=ambiguous_field
                select(mode+'-emitted-option',d,field,text)
        d=page('large');c=control(d)['select'];assert c['option_count']==25 and not c['options_complete']
        first=c['options'][0];assert first['option']=={'value':'0'},first
        select('outside-projection-label-refused',d,'label','Choice 0',2)
        select('outside-projection-emitted-value',d,'value','0')
        d=page('empty');a=select('empty-value',d,'value','');assert a['selection']['after']['value']==''
        d=obs('empty-feedback');assert 'selected 1 trusted=true' in d['content']['text']
        assert any(e['index']==['1'] for e in events.get('empty',[])),events
    d=page('privacy');password=control(d,'Password')
    call('password-fill',['act','fill',password['ref'],'--id',sid,'--text','fixture-password','--sensitive'])
    d=obs('privacy-before');assert control(d)['select']['options']==[]
    a=select('privacy-known-label',d,'label','全文识别')
    assert a['selection']['after']['value_redacted'] and 'value' not in a['selection']['after']
    assert 'full' not in json.dumps(a)
    d=obs('privacy-after')
    button=control(d,'Continue');call('continue',['act','click',button['ref'],'--id',sid])
    obs('continued')
    assert events and all(e['trusted']==['true'] for entries in events.values() for e in entries),events
    (root/'events.json').write_text(json.dumps(events,ensure_ascii=False,indent=2))
finally:
    if opened:
        call('close',['close','--id',sid]);call('closed',['session','status','--id',sid],2)
    server.shutdown();server.server_close()

assert hashlib.sha256(Path(binary).read_bytes()).hexdigest()==fingerprint, 'binary changed during acceptance'
(root/'verdict.json').write_text(json.dumps({'passed':True,'binary':binary,'sha256':fingerprint,'session':sid,'cases':len(records),'cleanup_verified':True,'scope':'native keyboard selection, exact option identity, application feedback and no-dispatch negatives'},indent=2))
print('Evidence:',root,flush=True)
