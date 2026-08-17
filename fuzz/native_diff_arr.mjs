import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'
import { readsArray } from './harness/reads.mjs'
// Array differential. Inserts scalars/null AND embedded shared types (Y.Text, Y.Map,
// Y.Array) as array elements, so the oracle exercises NATIVE construction of nested
// types in a Y.Array (the V1/V2 fuzz gate covers DECODE of these; this covers the Go
// side building them and re-encoding byte-exact). Deterministic.
function pick(rng,a){return a[(rng()*a.length)|0]}
function randText(rng,n){let s='';const C='abcdef';for(let i=0;i<n;i++)s+=C[(rng()*C.length)|0];return s}
// One embedded shared-type element.
//
// The Map arm used to set a SINGLE key, noted as "order-independent, so it stays byte-exact
// regardless of prelim key ordering" — a precaution that left prelim key ordering entirely
// unexercised (FR-006: a narrowing must be an open finding, not a harness constant). It now sets
// MULTIPLE keys, which is what actually exercises the prelim flush order.
function makeEmbed(rng){
  const k=(rng()*3)|0
  if(k===0){const s=randText(rng,1+((rng()*3)|0));return{y:new Y.Text(s),d:{ytype:'text',s}}}
  if(k===1){
    const n=2+((rng()*3)|0); const entries=[]
    for(let i=0;i<n;i++) entries.push(['k'+i, pick(rng,['x',1,true,null])])
    const m=new Y.Map(); for(const [kk,vv] of entries) m.set(kk,vv)
    return{y:m,d:{ytype:'map',entries}}
  }
  const items=[];const n=1+((rng()*2)|0);for(let i=0;i<n;i++)items.push(pick(rng,['a',2,false,null]));const ar=new Y.Array();ar.insert(0,items);return{y:ar,d:{ytype:'arr',items}}
}
function gen(seed,nOps){
  const rng=mulberry32(seed); const doc=new Y.Doc({gc:false}); doc.clientID=1
  const a=doc.getArray('a'); const ops=[]
  for(let i=0;i<nOps;i++){
    const len=a.length; const r=rng()
    if(len===0||r<0.55){
      const idx=len===0?0:(rng()*(len+1))|0
      let val,desc
      if(rng()<0.35){const e=makeEmbed(rng); val=e.y; desc=e.d}
      else{desc=pick(rng,['x','y','z',(rng()*100)|0,true,false,null]); val=desc}
      // push/unshift are distinct PUBLIC operations, not sugar for insert — they were the two
      // array mutators no generator drove (FR-005a).
      const how=rng()
      if(how<0.2){ a.push([val]); ops.push({op:'push',vals:[desc]}) }
      else if(how<0.35){ a.unshift([val]); ops.push({op:'unshift',vals:[desc]}) }
      else { a.insert(idx,[val]); ops.push({op:'insert',idx,vals:[desc]}) }
    } else {
      const idx=(rng()*len)|0; const dl=1+((rng()*(len-idx))|0)
      a.delete(idx,dl); ops.push({op:'delete',idx,len:dl})
    }
  }
  return {seed,ops,state:hex(Y.encodeStateAsUpdate(doc)),reads:readsArray(a)}
}
const s0=parseInt(process.argv[2]||'1'),n=parseInt(process.argv[3]||'1000'),o=parseInt(process.argv[4]||'15')
for(let s=s0;s<s0+n;s++) process.stdout.write(JSON.stringify(gen(s,o))+'\n')
