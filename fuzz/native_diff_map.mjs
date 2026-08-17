import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'
import { readsMap } from './harness/reads.mjs'
function gen(seed,nOps){
  const rng=mulberry32(seed); const doc=new Y.Doc({gc:false}); doc.clientID=1
  const m=doc.getMap('m'); const ops=[]
  for(let i=0;i<nOps;i++){
    const r=rng(); const key='abcde'[(rng()*5)|0]
    if(m.size===0||r<0.7){
      const v=[['x','y','z'][(rng()*3)|0],(rng()*100)|0,true,false,null][(rng()*5)|0]
      m.set(key,v); ops.push({op:'set',key,v})
    } else if(r<0.9){ m.delete(key); ops.push({op:'delete',key}) }
    else { m.clear(); ops.push({op:'clear'}) }
  }
  return {seed,ops,state:hex(Y.encodeStateAsUpdate(doc)),reads:readsMap(m)}
}
const s0=parseInt(process.argv[2]||'1'),n=parseInt(process.argv[3]||'1500'),o=parseInt(process.argv[4]||'15')
for(let s=s0;s<s0+n;s++) process.stdout.write(JSON.stringify(gen(s,o))+'\n')
