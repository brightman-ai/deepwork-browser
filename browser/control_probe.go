package browser

import (
	"encoding/json"
	"fmt"
)

// This one intrinsic predicate is shared by fixed capture/control/CSS readers.
// It returns no value and changes no DOM state.
const intrinsicSensitiveControlJS = `function(e){
 const type=String(e.type||'').toLowerCase(),ac=String(e.autocomplete||'').toLowerCase(),name=String(e.name||'').toLowerCase();
 return type==='password'||ac==='current-password'||ac==='new-password'||['passwd','password','pwd'].includes(name)||e.getAttribute('data-sensitive')==='true';
}`

// ControlProbeReadScript is a fixed CSS fact collector, not arbitrary eval.
// Private control values/text are null (unavailable), never successful empties.
func ControlProbeReadScript(selector string, redactAll bool) string {
	encoded, _ := json.Marshal(selector)
	return fmt.Sprintf(`(()=>{const sensitive=%s,redactAll=%t;const nodes=[...document.querySelectorAll(%s)];const privateNode=e=>sensitive(e)||(redactAll&&/^(INPUT|TEXTAREA)$/.test(e.tagName));const privateAncestor=e=>{for(let p=e;p;p=p.parentElement){if(privateNode(p))return true;}return false;};const privateText=e=>privateAncestor(e)||[...e.querySelectorAll('*')].some(privateNode);return {count:nodes.length,text:nodes.some(privateText)?null:nodes.map(e=>e.innerText||'').join('\n'),value:nodes.length===1&&!privateAncestor(nodes[0])&&typeof nodes[0].value==='string'?nodes[0].value:null};})()`, intrinsicSensitiveControlJS, redactAll, string(encoded))
}
