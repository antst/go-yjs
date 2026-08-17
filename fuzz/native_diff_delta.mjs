import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'
// ApplyDelta differential generator — stresses the formatting / value-rep paths: MULTI-KEY input
// attributes, `false` vs `null` (the ?? null fault line), 6 keys, varied/longer text, longer
// deltas. Deterministic; serializes the INPUT delta before applyDelta mutates the op objects.
const CHARS='abcdefghij'
const KEYS=['bold','italic','underline','color','size','link']
const VALS={bold:[true,false,null],italic:[true,false,null],underline:[true,null],color:['red','blue',null],size:[1,2,null],link:['x','y',null]}
function pick(rng,a){return a[(rng()*a.length)|0]}
function randText(rng,n){let s='';for(let i=0;i<n;i++)s+=CHARS[(rng()*CHARS.length)|0];return s}
function randAttr(rng){const o={};for(const k of KEYS){if(rng()<0.45)o[k]=pick(rng,VALS[k])}return o} // may be empty/multi-key
function serDelta(d){return d.map(op=>op.attributes?{...op,attributes:Object.entries(op.attributes)}:op)}
function gen(seed){
  const rng=mulberry32(seed); const doc=new Y.Doc({gc:false}); doc.clientID=1; const t=doc.getText('t')
  const base=[]
  const s0=randText(rng,3+((rng()*10)|0)); t.insert(0,s0); base.push({op:'insert',idx:0,s:s0})
  const nfmt=(rng()*3)|0
  for(let f=0;f<nfmt;f++){const L=t.length; if(L===0)break; const i=(rng()*L)|0,l=1+((rng()*(L-i))|0),a=randAttr(rng);t.format(i,l,a);base.push({op:'format',idx:i,len:l,attr:Object.entries(a)})}
  const len=t.length; const delta=[]; let pos=0
  while(pos<len){
    const r=rng(); const remain=len-pos
    if(r<0.4){const k=1+((rng()*remain)|0); const op={retain:k}; if(rng()<0.65)op.attributes=randAttr(rng); delta.push(op); pos+=k}
    else if(r<0.65){const k=1+((rng()*remain)|0); delta.push({delete:k}); pos+=k}
    else {const s=randText(rng,1+((rng()*3)|0)); const op={insert:s}; if(rng()<0.6)op.attributes=randAttr(rng); delta.push(op)}
  }
  const tail=(rng()*3)|0
  for(let i=0;i<tail;i++){const op={insert:randText(rng,1+((rng()*2)|0))}; if(rng()<0.5)op.attributes=randAttr(rng); delta.push(op)}
  const deltaSer=serDelta(delta)
  t.applyDelta(delta)
  return {seed,base,delta:deltaSer,state:hex(Y.encodeStateAsUpdate(doc))}
}
const s0=parseInt(process.argv[2]||'1'),n=parseInt(process.argv[3]||'20000')
let emitted=0,dropped=0
for(let s=s0;s<s0+n;s++){try{process.stdout.write(JSON.stringify(gen(s))+'\n');emitted++}catch(e){dropped++;process.stderr.write(`seed ${s} FAILED: ${e.stack||e}\n`)}}
process.stderr.write(`emitted=${emitted} dropped=${dropped}\n`)
// A dropped seed means gen() THREW — a real generation failure, not a skip. Failing
// here (rather than exiting 0 on a shrunk corpus) keeps the oracle's surfaced-errors
// contract: a bad seed becomes an oracle failure instead of silently reducing coverage.
if(dropped>0){process.stderr.write('FATAL: dropped seeds (generation threw)\n');process.exit(1)}
if(emitted===0){process.stderr.write('FATAL: empty corpus\n');process.exit(1)}
