// Serializable finite DOM checker, shared by Node negative tests and Chrome.
// A marker can exempt only a text-only SPAN. Never substring-delete API values.
export function scanRawChrome(root) {
  root ||= document.body;
  const raw=[],invalid=[],chrome=[];
  const banner='前端预览 · 数据均为 mock（接口未真正调用）';
  const walk=(node,trail='body')=>{
    if(node.nodeType===3) {const text=node.textContent||'';if(text.trim()&&text!==banner)chrome.push({position:trail,text});return;}
    if(node.nodeType!==1) return;
    const tag=node.tagName,id=node.id||node.getAttribute?.('id')||'';
    const here=trail+'/'+tag+(id?'#'+id:'');
    if(['SCRIPT','STYLE','NOSCRIPT','TEMPLATE'].includes(tag)||node.hidden) return;
    if(typeof getComputedStyle==='function') { const s=getComputedStyle(node);if(s.display==='none'||s.visibility==='hidden')return; }
    // Existing intentional bilingual hint and native language-name links only.
    if(node.matches?.('.lang-hint'))return;
    if(node.matches?.('#lang-switcher a[hreflang], #lang-switcher-login a[hreflang]'))return;
    const kids=Array.from(node.childNodes||node.children||[]);
    if(node.hasAttribute?.('data-raw-content')) {
      if(tag==='SPAN'&&!kids.some(n=>n.nodeType!==3)&&!(node.children?.length)) {raw.push({position:here,text:node.textContent||''});return;}
      invalid.push({position:here,tag,children:kids.map(n=>n.tagName||n.nodeType)});
      // Invalid markers do not exempt themselves or their children.
    }
    if(!kids.length&&node.textContent?.trim()&&node.textContent!==banner)chrome.push({position:here,text:node.textContent});
    for(const n of kids) walk(n,here);
  };
  walk(root);
  return {text:chrome.map(x=>x.text).join('\n'),chrome,raw,invalid};
}
export function chromeResidue(scan,lang) {
  const zhOnly=['总览','通知接收','已停用','确定停用','已删除','已吊销','暂无该状态的通知','最近收录'];
  return scan.chrome.filter(x=>(lang==='en'||lang==='es')?/[\u4e00-\u9fff\u3400-\u4dbf]/.test(x.text):lang==='ja'?zhOnly.some(s=>x.text.includes(s)):false);
}
