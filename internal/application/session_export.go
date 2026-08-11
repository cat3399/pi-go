package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type sessionExportData struct {
	Header  json.RawMessage   `json:"header"`
	Entries []json.RawMessage `json:"entries"`
	LeafID  *string           `json:"leafId"`
}

func (s *Service) ExportSession(ctx context.Context, id string) (SessionExport, error) {
	ctx = normalizeContext(ctx)
	if cause := context.Cause(ctx); cause != nil {
		return SessionExport{}, cause
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return SessionExport{}, err
	}
	if closeManager {
		defer manager.Close()
	}
	entries := manager.Entries()
	rawEntries := make([]json.RawMessage, len(entries))
	for index := range entries {
		rawEntries[index] = entries[index].RawJSON()
	}
	var leafID *string
	if value, ok := manager.LeafID(); ok {
		leafID = &value
	}
	payload, err := json.Marshal(sessionExportData{
		Header: manager.Header().RawJSON(), Entries: rawEntries, LeafID: leafID,
	})
	if err != nil {
		return SessionExport{}, fmt.Errorf("encode session export: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	html := strings.Replace(sessionExportHTML, "{{SESSION_DATA}}", encoded, 1)
	file, _ := manager.SessionFile()
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if base == "" || base == "." {
		base = manager.SessionID()
	}
	base = safeExportFilePart(base)
	if base == "" {
		return SessionExport{}, errors.New("session export has no valid file name")
	}
	return SessionExport{FileName: "pi-session-" + base + ".html", HTML: []byte(html)}, nil
}

var invalidExportFilePart = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeExportFilePart(value string) string {
	value = invalidExportFilePart.ReplaceAllString(value, "-")
	return strings.Trim(value, ".-")
}

// The export is deliberately self-contained and renderer-independent. Session
// data is base64 encoded, then all transcript content is inserted through DOM
// text nodes; prompts and tool output can never become executable markup.
const sessionExportHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="color-scheme" content="dark light">
  <title>Pi session</title>
  <style>
    :root{color-scheme:dark;--bg:#17181c;--panel:#1e2026;--panel2:#262932;--text:#e7e9ee;--muted:#989eaa;--line:#343844;--accent:#a78bfa;--user:#252a36;--error:#fca5a5}
    @media(prefers-color-scheme:light){:root{color-scheme:light;--bg:#f3f4f7;--panel:#fff;--panel2:#f4f5f8;--text:#20232a;--muted:#687080;--line:#d9dce3;--accent:#6d28d9;--user:#edf0f6;--error:#b91c1c}}
    *{box-sizing:border-box}html,body{margin:0;min-height:100%;background:var(--bg);color:var(--text);font:14px/1.55 ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
    body{display:grid;grid-template-columns:minmax(220px,300px) minmax(0,1fr)}aside{position:sticky;top:0;height:100vh;overflow:auto;border-right:1px solid var(--line);background:var(--panel);padding:18px 14px}
    main{min-width:0;padding:30px max(24px,5vw) 80px}.brand{font-weight:750;font-size:16px}.meta{margin-top:5px;color:var(--muted);font-size:12px;overflow-wrap:anywhere}.tree{margin-top:18px;display:flex;flex-direction:column;gap:4px}
    .tree button{width:100%;border:0;border-radius:7px;background:transparent;color:var(--muted);padding:7px 9px;text-align:left;cursor:pointer;font:12px/1.35 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
    .tree button:hover{background:var(--panel2);color:var(--text)}.tree button.active{background:var(--panel2);color:var(--accent)}header{max-width:920px;margin:0 auto 24px}.title{font-size:22px;font-weight:760;overflow-wrap:anywhere}.subtitle{color:var(--muted);margin-top:5px;overflow-wrap:anywhere}
    #messages{max-width:920px;margin:auto;display:flex;flex-direction:column;gap:14px}.message{border:1px solid var(--line);border-radius:10px;background:var(--panel);overflow:hidden}.message.user{background:var(--user)}
    .message-head{display:flex;justify-content:space-between;gap:12px;padding:8px 12px;border-bottom:1px solid var(--line);color:var(--muted);font-size:11px;text-transform:uppercase;letter-spacing:.06em}.message-body{padding:13px 14px;white-space:pre-wrap;overflow-wrap:anywhere}
    .block+.block{margin-top:12px}.thinking,.tool{border-left:3px solid var(--line);padding-left:10px;color:var(--muted)}details summary{cursor:pointer;color:var(--muted)}pre{margin:7px 0 0;padding:10px;border-radius:7px;background:var(--panel2);overflow:auto;white-space:pre-wrap;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace}.empty{color:var(--muted);text-align:center;padding:50px 10px}
    img{display:block;max-width:100%;max-height:520px;border-radius:7px;margin-top:8px}.error{color:var(--error)}
    @media(max-width:760px){body{display:block}aside{position:relative;height:auto;border-right:0;border-bottom:1px solid var(--line)}.tree{max-height:180px}main{padding:22px 14px 60px}}
  </style>
</head>
<body>
  <aside><div class="brand">Pi session</div><div id="side-meta" class="meta"></div><div id="tree" class="tree"></div></aside>
  <main><header><div id="title" class="title"></div><div id="subtitle" class="subtitle"></div></header><section id="messages"></section></main>
  <script>
  (()=>{
    "use strict";
    const bytes=Uint8Array.from(atob("{{SESSION_DATA}}"),c=>c.charCodeAt(0));
    const data=JSON.parse(new TextDecoder().decode(bytes));
    const header=data.header||{};
    const entries=Array.isArray(data.entries)?data.entries:[];
    const byId=new Map(); const children=new Map();
    for(const entry of entries){if(entry&&typeof entry.id==="string")byId.set(entry.id,entry)}
    for(const entry of entries){if(!entry||typeof entry.id!=="string")continue;const requestedParent=typeof entry.parentId==="string"?entry.parentId:"";const parent=requestedParent&&byId.has(requestedParent)?requestedParent:"";if(!children.has(parent))children.set(parent,[]);children.get(parent).push(entry)}
    const latestInfo=[...entries].reverse().find(e=>e&&e.type==="session_info"&&typeof e.name==="string"&&e.name.trim());
    const sessionTitle=latestInfo?latestInfo.name.trim():(header.id||"Pi session");
    document.title=sessionTitle+" · Pi"; document.getElementById("title").textContent=sessionTitle;
    document.getElementById("subtitle").textContent=header.cwd||"";
    document.getElementById("side-meta").textContent=(header.timestamp||"")+((header.id)?"\n"+header.id:"");
    const tree=document.getElementById("tree"), messages=document.getElementById("messages");
    function textOfContent(content){if(typeof content==="string")return content;if(!Array.isArray(content))return "";return content.filter(b=>b&&b.type==="text"&&typeof b.text==="string").map(b=>b.text).join("\n")}
    function labelFor(entry){if(entry.type==="message"&&entry.message){const text=textOfContent(entry.message.content).replace(/\s+/g," ").trim();return (entry.message.role||"message")+(text?" · "+text.slice(0,52):"")}return entry.type||"entry"}
    let activeLeaf=typeof data.leafId==="string"?data.leafId:(entries.length?entries[entries.length-1].id:null);
    function pathTo(id){const path=[],seen=new Set();let current=byId.get(id);while(current&&!seen.has(current.id)){seen.add(current.id);path.push(current);current=typeof current.parentId==="string"?byId.get(current.parentId):null}return path.reverse()}
    function addText(parent,text,className){const node=document.createElement("div");node.className=className||"block";node.textContent=text;parent.appendChild(node)}
    function addContent(parent,content){if(typeof content==="string"){addText(parent,content,"block");return}if(!Array.isArray(content)){addText(parent,JSON.stringify(content??"",null,2),"block");return}for(const block of content){if(!block)continue;if(block.type==="text"){addText(parent,block.text||"","block")}else if(block.type==="thinking"){const d=document.createElement("details");d.className="block thinking";const s=document.createElement("summary");s.textContent="Thinking";d.appendChild(s);addText(d,block.thinking||"","");parent.appendChild(d)}else if(block.type==="toolCall"){const d=document.createElement("details");d.className="block tool";const s=document.createElement("summary");s.textContent="Tool · "+(block.name||"");d.appendChild(s);const pre=document.createElement("pre");pre.textContent=JSON.stringify(block.arguments??{},null,2);d.appendChild(pre);parent.appendChild(d)}else if(block.type==="image"&&typeof block.data==="string"&&typeof block.mimeType==="string"&&block.mimeType.startsWith("image/")){const image=document.createElement("img");image.loading="lazy";image.alt="Session image";image.src="data:"+block.mimeType+";base64,"+block.data;parent.appendChild(image)}}}
    function renderEntry(entry){const message=entry.message;const role=message&&message.role?message.role:(entry.type||"entry");const card=document.createElement("article");card.className="message "+role;const head=document.createElement("div");head.className="message-head";const left=document.createElement("span");left.textContent=role;const right=document.createElement("span");right.textContent=entry.timestamp||"";head.append(left,right);card.appendChild(head);const body=document.createElement("div");body.className="message-body";
      if(entry.type==="message"&&message){if(message.role==="toolResult"){const d=document.createElement("details");d.open=false;const s=document.createElement("summary");s.textContent=(message.isError?"Tool error":"Tool result")+(message.toolName?" · "+message.toolName:"");d.appendChild(s);const pre=document.createElement("pre");pre.textContent=textOfContent(message.content)||JSON.stringify(message.content??{},null,2);d.appendChild(pre);body.appendChild(d)}else addContent(body,message.content)}
      else if(entry.type==="custom_message"&&entry.message)addContent(body,entry.message.content)
      else if(entry.type==="compaction")addText(body,entry.summary||"Compaction","")
      else if(entry.type==="branch_summary")addText(body,entry.summary||"Branch summary","")
      else if(entry.type==="session_info")addText(body,"Session title: "+(entry.name||""),"")
      else addText(body,JSON.stringify(entry,null,2),"");card.appendChild(body);messages.appendChild(card)}
    function render(){messages.replaceChildren();const path=activeLeaf?pathTo(activeLeaf):[];const visible=path.filter(e=>e.type!=="label");if(!visible.length){const empty=document.createElement("div");empty.className="empty";empty.textContent="No messages in this session.";messages.appendChild(empty)}else visible.forEach(renderEntry);for(const button of tree.querySelectorAll("button"))button.classList.toggle("active",button.dataset.id===activeLeaf)}
    const stack=[...(children.get("")||[])].reverse();const seen=new Set();while(stack.length){const entry=stack.pop();if(!entry||seen.has(entry.id))continue;seen.add(entry.id);const button=document.createElement("button");button.type="button";button.dataset.id=entry.id;button.textContent=labelFor(entry);button.title=button.textContent;button.style.paddingLeft=(9+Math.min(pathTo(entry.id).length-1,12)*10)+"px";button.addEventListener("click",()=>{activeLeaf=entry.id;render()});tree.appendChild(button);const next=children.get(entry.id)||[];for(let i=next.length-1;i>=0;i--)stack.push(next[i])}
    render();
  })();
  </script>
</body>
</html>`
