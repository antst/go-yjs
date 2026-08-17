import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'
import { readsXml } from './harness/reads.mjs'
const TAGS=['div','span','p'], AKEYS=['a','b','c']
function gen(seed,nOps){
  const rng=mulberry32(seed); const doc=new Y.Doc({gc:false}); doc.clientID=1
  const f=doc.getXmlFragment('f'); const ops=[]
  for(let i=0;i<nOps;i++){
    const len=f.length; const r=rng()
    if(len===0||r<0.5){ const idx=len===0?0:(rng()*(len+1))|0; const tag=TAGS[(rng()*3)|0]
      // push / unshift / insertAfter are distinct public mutators that no generator drove.
      const how=rng()
      if(how<0.15){ f.push([new Y.XmlElement(tag)]); ops.push({op:'pushElem',tag}) }
      else if(how<0.3){ f.unshift([new Y.XmlElement(tag)]); ops.push({op:'unshiftElem',tag}) }
      else if(how<0.4 && len>0){ const ref=f.get((rng()*len)|0)
        f.insertAfter(ref,[new Y.XmlElement(tag)]); ops.push({op:'insertAfterElem',refIdx:f.toArray().indexOf(ref),tag}) }
      else { f.insert(idx,[new Y.XmlElement(tag)]); ops.push({op:'insElem',idx,tag}) } }
    else if(r<0.8){ const idx=(rng()*len)|0; const el=f.get(idx)
      if(el instanceof Y.XmlElement){ const k=AKEYS[(rng()*3)|0]
        // FR-016 bar (b): a binary attribute value is the ONLY thing that exercises the []uint8
        // arm of xmlAttrValueString, where Go rendered "[1 2 3]" against the reference's "1,2,3".
        // Without it that defect was fixed and unit-tested but NOT reachable by the xml surface,
        // so a recurrence would have been invisible to the oracle. Uint8Array does not survive
        // JSON.stringify as an array, so it is tagged for the Go replay to reconstruct.
        const pick=(rng()*5)|0
        let v, wire
        if(pick===4){ const n=1+((rng()*3)|0); const b=[]
          for(let j=0;j<n;j++) b.push((rng()*256)|0)
          v=new Uint8Array(b); wire={__bin:b} }
        else { v=[['x','y'][(rng()*2)|0],(rng()*50)|0,true,null][pick]; wire=v }
        if(v===null){ el.removeAttribute(k); ops.push({op:'rmAttr',idx,k}) }
        else { el.setAttribute(k,v); ops.push({op:'setAttr',idx,k,v:wire}) } } }
    else { const idx=(rng()*len)|0; f.delete(idx,1); ops.push({op:'del',idx}) }
  }
  return {seed,ops,state:hex(Y.encodeStateAsUpdate(doc)),str:f.toString(),reads:readsXml(f)}
}
const s0=parseInt(process.argv[2]||'1'),n=parseInt(process.argv[3]||'1500'),o=parseInt(process.argv[4]||'15')
for(let s=s0;s<s0+n;s++) process.stdout.write(JSON.stringify(gen(s,o))+'\n')
