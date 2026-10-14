const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const page=fs.readFileSync(require('node:path').join(__dirname,'../page.html'),'utf8');
const script=page.split('<script>')[1].split('</script>')[0].split('\nrenderProfiles();selectView(view);')[0];
function setup(){
 const elements=new Map(),storage=new Map(),calls=[],timers=new Map(),streams=[];let clock=0,nodeCounter=0;
 class EventSource{constructor(url){this.url=url;this.listeners={};streams.push(this)}addEventListener(type,fn){this.listeners[type]=fn}close(){this.closed=true}emit(type,data){this.listeners[type]?.({data:JSON.stringify(data)})}}
 function element(id){if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',style:{},children:[],replaceChildren(){this.children=[];this.innerHTML=''},querySelectorAll(){return this.children.filter(n=>['VIDEO','AUDIO'].includes(n.tagName))},pause(){this.paused=true},load(){this.loaded=true},firstChild:{textContent:''},classList:{toggle(){}},clientWidth:0,hidden:false,disabled:false,setAttribute(){},removeAttribute(){},addEventListener(){},contains(){return false},showModal(){},close(){},focus(){},select(){},appendChild(node){this.children.push(node);node.parentElement=this;return node}});return elements.get(id)}
 const config={down_kbps:400,up_kbps:200,delay_ms:200,jitter_ms:50,loss_pct:1,window_kb:128,outage:'',blacklist_on:false,blacklist:[],whitelist:[]};
 const state={config,presets:[],devices:[],lanes:[],apps:[],events:[],event_seq:0,adb:false,_appFilter:''};
 const response=data=>({ok:true,text:async()=>JSON.stringify(data)});
 let handler=(url,options)=>response(url.startsWith('/api/config')?{...config,...JSON.parse(options.body)}:state);
 const context=vm.createContext({
  document:{createElement:tag=>{const node=element('created-'+(++nodeCounter));node.tagName=tag.toUpperCase();return node},head:{appendChild(){}},getElementById:element,querySelectorAll:()=>[],activeElement:null,hidden:false,addEventListener(){}},
  window:{getSelection:()=>null,addEventListener(){}},navigator:{clipboard:{writeText:async()=>{}}},
  localStorage:{getItem:key=>storage.get(key),setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
  setTimeout:fn=>{timers.set(++clock,fn);return clock},clearTimeout:key=>timers.delete(key),setInterval(){},
  fetch:async(url,options)=>{calls.push({url,options});return handler(url,options)},
  EventSource,AbortController,URLSearchParams,URL,Blob,Date,TextDecoder,Uint8Array,location:{href:'http://127.0.0.1:8899/'},devicePixelRatio:1,confirm:()=>true,prompt:()=>null
 });
 const run=code=>vm.runInContext(code,context);run(script);context.testState=state;run('state=testState;displayState=state;renderConfig()');
 return {run,element,storage,calls,timers,streams,state,response,setHandler:fn=>handler=fn};
}
test('快速连续调整参数合并提交，不丢失前一个字段',async()=>{
 const h=setup();h.run("send('down_kbps',300);send('delay_ms',250)");await h.run('flushParams()');
 const posts=h.calls.filter(c=>c.url==='/api/config');assert.equal(posts.length,1);assert.deepEqual(JSON.parse(posts[0].options.body),{down_kbps:300,delay_ms:250});
});
test('保存失败保留待提交参数，并且可以重试',async()=>{
 const h=setup();h.setHandler(()=>({ok:false,text:async()=>JSON.stringify({error:'写入失败'})}));h.run("send('up_kbps',123)");
 assert.equal(await h.run('flushParams()'),false);assert.equal(h.run('pending.up_kbps'),123);assert.match(h.element('quickConfigStatus').innerHTML,/写入失败/);
 h.setHandler(()=>h.response({...h.state.config,up_kbps:123}));assert.equal(await h.run('flushParams()'),true);assert.equal(h.run('Object.keys(pending).length'),0);
});
test('失去焦点、刷新和重新打开页面都保留名单草稿，保存时去重',async()=>{
 const h=setup();h.element('blacklist').value='api.example.com,api.example.com\ncdn.example.com';h.run("draftChanged('blacklist');document.activeElement=null;renderConfig()");
 assert.match(h.element('blacklist').value,/cdn.example.com/);assert.ok(h.storage.has('wn_draft_blacklist'));
 h.run('listsReady=false;dirty.blacklist=false');h.element('blacklist').value='';h.run('renderConfig()');assert.match(h.element('blacklist').value,/cdn.example.com/);
 await h.run("saveList('blacklist')");const post=h.calls.find(c=>c.url==='/api/config');assert.deepEqual(JSON.parse(post.options.body).blacklist,['api.example.com','cdn.example.com']);assert.equal(h.storage.has('wn_draft_blacklist'),false);
});
test('较早返回的历史请求不能覆盖新的筛选结果',async()=>{
 const h=setup(),requests=[];h.run("view='history'");h.setHandler(()=>new Promise(resolve=>requests.push(resolve)));
 const old=h.run('refreshConns(true)');h.run("fApp='com.new.app'");const current=h.run('refreshConns(true)');
 const data=count=>({rows:[],summary:{count,active:0,failed:0,down:0,up:0,avg_ms:0,shaped:0,bypassed:0},next_before:0});
 requests[1](h.response(data(2)));await current;requests[0](h.response(data(99)));await old;assert.equal(h.run('connData.summary.count'),2);
});
test('暂停期间显示保持不变，恢复后更新',async()=>{
 const h=setup();h.run("$('pause').onclick()");h.setHandler(()=>h.response({...h.state,event_seq:4,events:['new event']}));await h.run('refresh()');
 assert.equal(h.run('displayState.events.length'),0);assert.match(h.element('pauseInfo').textContent,/新增 4 条/);
 h.run("$('pause').onclick()");assert.equal(h.run('displayState.events[0]'),'new event');
});
test('SSE 持续接收数据；关闭实时跟随或暂停列表只冻结显示，新请求仅作提示',()=>{
 const h=setup();h.run("view='monitor';$('evFollow').checked=false;connectStateStream()");const stream=h.streams[0];
 const row={id:1,scope:'http',status:'active',text:'12:00:00 first'};
 stream.emit('state',{...h.state,revision:1,event_details:[row]});assert.equal(h.run('eventMenuRows[0].id'),1);
 stream.emit('state',{...h.state,revision:2,event_details:[{...row,id:2},row]});assert.equal(h.run('state.revision'),2);assert.equal(h.run('eventMenuRows.length'),1);assert.equal(h.run('requestNewEvents.size'),1);assert.equal(h.calls.length,0);
 h.run("$('pauseRequests').onclick()");stream.emit('state',{...h.state,revision:3,event_details:[{...row,id:3},row]});assert.equal(h.run('state.revision'),3);assert.equal(h.run('eventMenuRows[0].id'),1);assert.equal(stream.closed,undefined);
 h.run("$('pauseRequests').onclick()");assert.equal(h.run('eventMenuRows[0].id'),3);
 h.run("$('evFollow').checked=true;$('evFollow').onchange()");stream.emit('state',{...h.state,revision:4,event_details:[{...row,id:4},{...row,id:3},row]});assert.equal(h.run('eventMenuRows[0].id'),4);assert.equal(h.run('eventMenuRows.length'),3);
 assert.doesNotMatch(page,/setInterval\([^\n]*(?:refresh\(\)|pollRealtime|pollDashboard)/);
});
test('SSE 保留请求数据处理流量快照，忽略旧版本，切换 App 关闭旧连接，重连允许服务重启',()=>{
 const h=setup();h.run('connectStateStream()');const first=h.streams[0];
 first.emit('state',{...h.state,revision:10,event_seq:10,event_details:[{id:10,scope:'http',text:'first'}]});
 first.emit('metrics',{...h.state,revision:10,event_seq:0,event_details:null,events:null});assert.equal(h.run('state.event_details[0].id'),10);assert.equal(h.run('state.event_seq'),10);
 first.emit('state',{...h.state,revision:9,event_details:[]});assert.equal(h.run('state.event_details[0].id'),10);
 first.onopen();first.emit('state',{...h.state,revision:0,event_details:[]});assert.equal(h.run('state.revision'),0);
 h.run("fApp='com.other';connectStateStream()");assert.equal(first.closed,true);assert.match(h.streams[1].url,/app=com.other/);
 first.emit('state',{...h.state,revision:99,event_details:[]});assert.equal(h.run('state.revision'),0);
 h.run('closeStateStream();document.hidden=true;connectStateStream()');assert.equal(h.streams.length,2);
});
test('复制请求地址只提取脱敏 URL，不混入方法和 App 信息',()=>{
 const h=setup();assert.equal(h.run("eventAddress('12:00:00 [phone] [HTTP][App 日志] POST https://api.example/room?id=REDACTED · com.example.app')"),'https://api.example/room?id=REDACTED');
 assert.equal(h.run("eventAddress('[HTTPS 隧道][代理] CONNECT api.example:443 · com.example.app')"),'');
});
test('导入方案拒绝非法丢包率和错误名单类型',()=>{
 const h=setup();assert.throws(()=>h.run('profileConfig({...state.config,loss_pct:101})'),/loss_pct/);assert.throws(()=>h.run("profileConfig({...state.config,whitelist:['valid.example',42]})"),/名单格式/);
});
test('无效时间范围禁止导出，避免导出错误范围的数据',async()=>{
 const h=setup();h.run("view='history'");h.element('hSince').value='2026-10-12T10:00';h.element('hUntil').value='2026-10-11T10:00';await h.run('refreshConns(true)');assert.match(h.element('connError').textContent,/时间范围/);assert.equal(h.calls.length,0);
});
test('较早的保存失败不会恢复并覆盖较新的参数值',async()=>{
 const h=setup(),requests=[];h.setHandler(()=>new Promise(resolve=>requests.push(resolve)));
 h.run("send('down_kbps',300)");const first=h.run('flushParams()');await new Promise(resolve=>setImmediate(resolve));
 h.run("send('down_kbps',400)");const second=h.run('flushParams()');
 requests[0]({ok:false,text:async()=>JSON.stringify({error:'older failure'})});await first;await new Promise(resolve=>setImmediate(resolve));
 requests[1](h.response({...h.state.config,down_kbps:400}));await second;
 assert.equal(h.run('state.config.down_kbps'),400);assert.equal(h.run('Object.keys(pending).length'),0);
});
test('正在保存的参数失败时，不继续应用其他配置',async()=>{
 const h=setup();let finish;h.setHandler(()=>new Promise(resolve=>finish=resolve));h.run("send('up_kbps',321)");
 const first=h.run('flushParams()');await new Promise(resolve=>setImmediate(resolve));const change=h.run("changeConfig({outage:'stall'})");
 finish({ok:false,text:async()=>JSON.stringify({error:'failed'})});await first;assert.equal(await change,false);assert.equal(h.calls.length,1);
});
test('顶部只保留三个工作页面，请求事件留在工作台，设置独立打开',()=>{
 const h=setup();const nav=page.match(/<nav[\s\S]*?<\/nav>/)[0];assert.deepEqual([...nav.matchAll(/data-view="(.*?)"/g)].map(m=>m[1]),['monitor','history','logs']);
 h.element('evSearch').value='demo.svga';h.run("selectView('monitor');selectView('history');selectView('monitor')");
 assert.equal(h.element('eventsCard').parentElement,h.element('monitorEvents'));assert.equal(h.element('evSearch').value,'demo.svga');
 h.run("$('openSettings').onclick()");assert.equal(h.run('view'),'monitor');
});
test('全局参数合并保存，并在当前页显示失败重试',async()=>{
 const h=setup();h.element('q_down_kbps').onchange({target:{value:'888'}});await h.run('flushParams()');assert.equal(JSON.parse(h.calls[0].options.body).down_kbps,888);
 h.setHandler(()=>({ok:false,text:async()=>JSON.stringify({error:'offline'})}));h.element('q_up_kbps').onchange({target:{value:'111'}});await h.run('flushParams()');assert.match(h.element('quickConfigStatus').innerHTML,/offline/);assert.match(h.element('quickConfigStatus').innerHTML,/重试/);
});
test('请求状态过滤、标签及复制内容保持一致，隧道与日志不误判',()=>{
 const h=setup();h.state.event_details=[{text:'12:00:00 [phone] [HTTP][代理] GET http://example.com',status:'active',scope:'http'},{text:'12:00:00 [phone] [HTTP][代理] GET http://example.com/timeout',status:'timeout',scope:'http',detail:'i/o timeout'},{text:'12:00:00 [phone] [HTTPS 隧道][代理] CONNECT example.com:443',status:'complete',scope:'tunnel'},{text:'12:00:00 [phone] [资源][App 日志] https://example.com/a.mp4',status:'unknown',scope:'log'}];
 h.state.events=h.state.event_details.map(e=>e.text);h.run('renderEvents()');assert.match(h.element('events').innerHTML,/请求中/);assert.match(h.element('events').innerHTML,/隧道已结束/);assert.doesNotMatch(h.element('events').innerHTML,/结果未知/);
 h.element('evStatus').value='errors';assert.equal(h.run('filteredEvents().length'),1);assert.match(h.run('filteredEvents().map(eventText).join("\\n")'),/请求失败（超时）.*i\/o timeout/);
 h.state.event_details[0].status='complete';h.element('evStatus').value='active';assert.equal(h.run('filteredEvents().length'),0);
});
test('工作台共用设备操作和作用范围面板，切页不会丢失包名、目标手机或名单草稿',()=>{
 const h=setup();h.run("toolSerials=new Set(['phoneA']);toolSelectionReady=true");h.element('clrPkg').value='com.example.app';h.element('blacklist').value='api.example.com';h.run("draftChanged('blacklist');selectView('monitor')");
 assert.equal(h.element('deviceTools').parentElement,h.element('monitorDeviceTools'));assert.equal(h.element('scopeCard').parentElement,h.element('monitorScope'));
 h.run("selectView('history');selectView('monitor')");assert.equal(h.element('clrPkg').value,'com.example.app');assert.equal(h.element('blacklist').value,'api.example.com');
 h.state.devices=[{serial:'phoneA',state:'device',model:'phone',enabled:true,busy:'清空数据'}];h.run('renderClear()');assert.equal(h.element('clrBtn').disabled,true);assert.match(h.element('toolDeviceActions').innerHTML,/重启 Wi-Fi/);assert.match(h.element('toolDeviceActions').innerHTML,/disabled/);
});

test('多手机共用操作面板，只向勾选的手机批量提交，分别报告失败',async()=>{
 const h=setup();h.state.devices=[{serial:'A',model:'phone A',state:'device',enabled:true},{serial:'B',model:'phone B',state:'device'},{serial:'C',model:'phone C',state:'device'}];
 h.run("toolSerials=new Set(['A','B']);toolSelectionReady=true;renderClear()");assert.match(h.element('toolSelection').textContent,/已选 2 台/);
 h.setHandler((url)=>url.includes('/device/B/')?({ok:false,text:async()=>JSON.stringify({error:'phone B busy'})}):h.response(url.startsWith('/api/device/')?{ok:true}:h.state));
 await h.run("submitToolAction('reconnect',{},toolTargets())");
 assert.deepEqual(h.calls.filter(c=>c.url.startsWith('/api/device/')).map(c=>c.url),['/api/device/A/reconnect','/api/device/B/reconnect']);assert.match(h.element('clrMsg').textContent,/phone A（A）：已提交/);assert.match(h.element('clrMsg').textContent,/phone B busy/);
});
test('批量清空一次确认全部目标，取消不发送；忙碌或离线手机不能被静默略过',async()=>{
 const h=setup();h.state.devices=[{serial:'A',model:'phone A',state:'device'},{serial:'B',model:'phone B',state:'device'}];h.run("toolSerials=new Set(['A','B']);toolSelectionReady=true;ask=async message=>{globalMessage=message;return null}");
 h.element('clrPkg').value='com.example.app';await h.run('clearApp(true)');assert.equal(h.calls.length,0);assert.match(h.run('globalMessage'),/phone A（A）/);assert.match(h.run('globalMessage'),/phone B（B）/);
 h.run('ask=async()=>true');await h.run('clearApp(true)');const calls=h.calls.filter(c=>c.url.startsWith('/api/device/'));assert.equal(calls.length,2);assert.deepEqual(JSON.parse(calls[1].options.body),{pkg:'com.example.app',launch:true});
 h.state.devices[1].busy='清空';h.run('state=testState;renderClear()');assert.equal(h.element('clrBtn').disabled,true);assert.equal(h.run('toolTargets()'),null);
 h.state.devices.splice(1);assert.equal(h.run('toolTargets()'),null);h.run("selectView('history');selectView('monitor')");assert.equal(h.run('toolSerials.size'),2);
});
test('日志暂停只冻结内容，采集状态仍更新，恢复后可复制筛选结果',async()=>{
 const h=setup();h.run("view='logs';let copiedLog='';navigator.clipboard.writeText=async text=>{copiedLog=text}");
 const row={id:1,device:'Phone',serial:'one',time:'10-12 01:00:00',pid:'123',tid:'124',level:'E',tag:'Crash',app:'com.example',message:'timeout <script>'};
 let data={rows:[row],total:1,sessions:[{serial:'one',device:'Phone',enabled:true,status:'running'}]};h.setHandler(()=>h.response(data));await h.run('refreshLogs()');
 assert.match(h.element('logLines').innerHTML,/&lt;script&gt;/);h.run("$('logPause').onclick()");
 data={rows:[{...row,id:2,message:'new'}],total:2,sessions:[{serial:'one',device:'Phone',enabled:true,status:'retrying',error:'offline'}]};await h.run('refreshLogs()');
 assert.equal(h.run('logData.rows[0].id'),1);assert.match(h.element('logSessionStatus').textContent,/offline/);
 h.run("$('logPause').onclick()");await new Promise(resolve=>setImmediate(resolve));assert.equal(h.run('logData.rows[0].id'),2);
 await h.run("$('logCopy').onclick()");assert.match(h.run('copiedLog'),/com.example: new/);
});
test('日志筛选切换拒绝旧响应，清空只发送所选手机范围',async()=>{
 const h=setup(),requests=[];h.run("view='logs'");h.setHandler(()=>new Promise(resolve=>requests.push(resolve)));
 const old=h.run('refreshLogs()');h.element('logSerial').value='two';h.run('logFiltersChanged()');
 requests[0](h.response({rows:[{id:99}],sessions:[],total:1}));await old;assert.match(h.calls[1].url,/serial=two/);
 requests[1](h.response({rows:[],sessions:[],total:0}));await new Promise(resolve=>setImmediate(resolve));assert.equal(h.run('logData.rows.length'),0);
 h.setHandler(url=>h.response(url.startsWith('/api/logs/clear')?{ok:true}:{rows:[],sessions:[],total:0}));await h.run("$('logClear').onclick()");
 assert.ok(h.calls.some(c=>c.url==='/api/logs/clear?serial=two'));assert.equal(h.calls.some(c=>c.url.includes('/api/device/')),false);
});
test('日志采集复用多手机选择，逐台报告失败，停止全部包含未勾选采集手机',async()=>{
 const h=setup();h.state.adb=true;h.state.devices=[{serial:'one',model:'Phone one',state:'device'},{serial:'two',model:'Phone two',state:'device'}];
 h.run("view='logs';toolSerials=new Set(['one','two'])");
 const sessions=[{serial:'one',device:'Phone one',enabled:true,status:'running'},{serial:'three',device:'Phone three',enabled:true,status:'retrying'}];
 h.setHandler(url=>url==='/api/logs/two/start'?{ok:false,text:async()=>'device offline'}:h.response(url.startsWith('/api/logs?')?{rows:[],total:0,sessions}:{ok:true}));
 await h.run("logAction('start')");assert.ok(h.calls.some(c=>c.url==='/api/logs/one/start'));assert.ok(h.calls.some(c=>c.url==='/api/logs/two/start'));assert.match(h.element('logActionResult').textContent,/Phone two（two）：device offline/);
 h.run("toolSerials=new Set(['one'])");await h.run("logAction('stop',true)");assert.ok(h.calls.some(c=>c.url==='/api/logs/three/stop'));
 h.element('logApp').value='com.example';h.element('logSearch').value='timeout';h.element('logLevel').value='W';assert.match(h.run('logQuery().toString()'),/app=com.example&level=W&q=timeout/);
});
test('两处包名选择共用组件，操作区轮询不重建候选，选择后保存包名',()=>{
 const h=setup();h.state.apps=[{app:'com.zeta.app'},{app:'com.alpha.app'}];h.element('clrPkg').value='';h.run("openPackagePicker('clrPkg')");const old=h.element('toolPkgOptions').innerHTML;
 h.state.apps.reverse();h.state.apps.push({app:'com.new.app'});h.run('renderPackageOptions();renderClear()');assert.equal(h.element('toolPkgOptions').innerHTML,old);
 h.run("$('clrPkg').onkeydown({key:'ArrowDown',preventDefault(){}});$('clrPkg').onkeydown({key:'Enter',preventDefault(){}})");assert.equal(h.element('clrPkg').value,'com.alpha.app');assert.equal(h.storage.get('wn_clr_pkg'),'com.alpha.app');assert.equal(h.element('toolPkgOptions').hidden,true);
 h.run('renderClear()');assert.equal(h.element('clrPkg').value,'com.alpha.app');
 h.element('clrPkg').value='unmatched';h.run("$('clrPkg').oninput();$('clrPkg').onkeydown({key:'ArrowDown',preventDefault(){}})");assert.match(h.element('toolPkgOptions').innerHTML,/没有匹配/);h.run("$('clrPkg').onkeydown({key:'Escape'})");assert.equal(h.element('toolPkgOptions').hidden,true);
});
test('日志包名下拉展开时轮询不重建候选，键盘确认后保留筛选',async()=>{
 const h=setup();h.state.apps=[{app:'com.first.app'}];h.run("view='logs';logData={rows:[{app:'com.logs.only:worker'}],total:1,sessions:[]};openLogPackages()");const old=h.element('logPkgOptions').innerHTML;
 h.state.apps=[{app:'com.new.app'}];h.setHandler(()=>h.response({rows:[],total:0,sessions:[]}));await h.run('refreshLogs()');h.run('renderClear()');
 assert.equal(h.element('logPkgOptions').innerHTML,old);assert.equal(h.run('packagePickers.logApp.open'),true);
 h.run("$('logApp').onkeydown({key:'ArrowDown',preventDefault(){}});$('logApp').onkeydown({key:'ArrowDown',preventDefault(){}});$('logApp').onkeydown({key:'Enter',preventDefault(){}})");
 assert.equal(h.element('logApp').value,'com.first.app');assert.equal(h.element('logPkgOptions').hidden,true);
 await new Promise(resolve=>setImmediate(resolve));await h.run('refreshLogs()');assert.equal(h.element('logApp').value,'com.first.app');assert.ok(h.calls.some(c=>c.url.includes('app=com.first.app')));
 h.element('logApp').value='com.first';h.run('openLogPackages()');assert.match(h.element('logPkgOptions').innerHTML,/com.first.app/);assert.doesNotMatch(h.element('logPkgOptions').innerHTML,/data-package="com.first"/);
 h.element('logApp').value='logs';h.run('logData={rows:[{app:"com.logs.only:worker"}],total:1,sessions:[]};openLogPackages()');assert.match(h.element('logPkgOptions').innerHTML,/com.logs.only/);assert.doesNotMatch(h.element('logPkgOptions').innerHTML,/com.new.app/);
 h.run("$('logApp').onkeydown({key:'Escape'})");assert.equal(h.element('logPkgOptions').hidden,true);
});

test('请求始终按观测范围筛选，取消实时跟随后仍能切换范围并复制内容',async()=>{
 const h=setup();const row={text:'12:00:00 [Same model] [HTTP][代理] GET http://api.example.com/a · com.example.app · 弱网',serial:'A',app:'com.example.app',host:'api.example.com',status:'active',scope:'http'};
 h.state.event_details=[row,{...row,serial:'B'},{...row,app:'com.other.app'},{text:'12:00:00 tool event'}];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("fSerial='A';fApp='com.example.app';renderEvents()");assert.equal(h.run('filteredEvents().length'),1);
 assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/com.example.app/);assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 const menu=h.run('eventMenuContent(filteredEvents()[0])');assert.match(menu,/data-copy=/);assert.match(menu,/com.example.app/);assert.match(menu,/请求中/);
 h.element('evFollow').checked=false;h.run("$('evFollow').onchange()");assert.equal(h.run('filteredEvents().length'),1);assert.match(h.run('eventContent(filteredEvents()[0])'),/data-app="com.example.app"/);assert.match(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 h.run("fSerial='';fApp='';renderEvents()");assert.equal(h.run('filteredEvents().length'),3);
});

test('HTTPS 轮询保留域名草稿，保存失败也不丢失，成功使用返回设置',async()=>{
 const h=setup();h.state.https={enabled:false,domains:['old.example'],fingerprint:'ABCD',expires:'2036-10-12',directory:'/local/weaknet'};
 h.run('renderHTTPS()');assert.equal(h.element('httpsDomains').value,'old.example');
 h.element('httpsDomains').value='api.example， cdn.example';h.element('httpsEnabled').checked=true;h.run("$('httpsDomains').oninput();renderHTTPS()");
 assert.equal(h.element('httpsDomains').value,'api.example， cdn.example');
 h.setHandler(()=>({ok:false,text:async()=>JSON.stringify({error:'disk full'})}));await h.run("$('saveHTTPS').onclick()");
 assert.equal(h.element('httpsDomains').value,'api.example， cdn.example');assert.equal(h.run('httpsDirty'),true);
 h.setHandler(()=>h.response({...h.state.https,enabled:true,domains:['api.example','cdn.example']}));await h.run("$('saveHTTPS').onclick()");
 assert.deepEqual(JSON.parse(h.calls.at(-1).options.body),{enabled:true,domains:['api.example','cdn.example'],reconnect:true});assert.equal(h.run('httpsDirty'),false);assert.equal(h.element('httpsDomains').value,'api.example, cdn.example');
});

test('工作台域名快捷添加保存并启用解密，失败保留草稿且不改已保存列表',async()=>{
 const h=setup();h.state.https={enabled:false,domains:['old.example'],fingerprint:'ABCD',expires:'2036-10-12',directory:'/local/weaknet'};h.run('renderHTTPS()');
 h.setHandler((url,options)=>h.response({...h.state.https,...JSON.parse(options.body)}));
 assert.equal(await h.run("addHTTPSDomain('API.Example.')"),true);
 assert.deepEqual(JSON.parse(h.calls[0].options.body),{enabled:true,domains:['old.example','api.example'],reconnect:true});assert.match(h.element('httpsDomainChips').innerHTML,/api.example/);assert.equal(h.run("httpsCovers('cdn.api.example')"),true);assert.equal(h.run("httpsCovers('evilapi.example')"),false);
 h.setHandler(()=>({ok:false,text:async()=>JSON.stringify({error:'disk full'})}));assert.equal(await h.run("addHTTPSDomain('new.example')"),false);
 assert.match(h.element('httpsDomains').value,/new.example/);assert.doesNotMatch(h.element('httpsDomainChips').innerHTML,/new.example/);assert.equal(h.run('httpsDirty'),true);
});
test('请求事件提供域名名单和解密操作，所有请求事件可打开详情',()=>{
 const h=setup();h.state.https={enabled:true,domains:['api.example']};h.state.event_details=[{id:1,text:'12:00:00 [phone] [HTTP][代理] GET https://api.example/a · com.example.app · HTTPS 解密 · 弱网',host:'api.example',status:'complete',scope:'http',decrypted:true},{id:2,text:'12:00:01 [phone] [HTTP][代理] GET https://other.example/a',host:'other.example',status:'active',scope:'http'}];h.state.events=h.state.event_details.map(e=>e.text);h.run('renderEvents()');
 assert.match(h.element('events').innerHTML,/data-request-detail="1"/);assert.match(h.element('events').innerHTML,/data-request-detail="2"/);const menu=h.run('eventMenuContent(filteredEvents()[1])');assert.match(menu,/data-list="blacklist" data-item="other.example"/);assert.match(menu,/data-https="other.example"/);assert.match(h.element('events').innerHTML,/data-event-more="1"/);assert.doesNotMatch(h.element('events').innerHTML,/data-list=/);assert.match(h.element('events').innerHTML,/HTTPS 已解密/);
 h.element('evType').value='decrypted';assert.equal(h.run('filteredEvents().length'),1);
});
test('请求详情拒绝旧响应覆盖新请求，并正确显示文本正文与脱敏头',async()=>{
 const h=setup(),pending=[];h.setHandler(()=>new Promise(resolve=>pending.push(resolve)));
 const first=h.run('openRequestDetail(1)'),second=h.run('openRequestDetail(2)');
 const detail=(id,body)=>({id,method:'POST',url:'https://api.example/'+id,status:'complete',status_code:200,protocol:'HTTP/2.0',duration_ms:24,device:'phone',serial:'A',app:'com.example.app',request:{headers:{Authorization:['REDACTED']},body:'{"room":123}',bytes:12,complete:true,truncated:false},response:{headers:{'Content-Type':['application/json']},body,bytes:20,complete:true,truncated:false}});
 pending[1](h.response(detail(2,'{"ok":true}')));await second;pending[0](h.response(detail(1,'old data')));await first;
 assert.equal(h.element('requestDetailURL').textContent,'https://api.example/2');assert.match(h.element('responseBody').textContent,/"ok": true/);assert.equal(h.element('requestHeaders').textContent,'Authorization: REDACTED');assert.match(h.run("inspectionText(requestDetailData,'response')"),/HTTP 200 HTTP\/2.0/);assert.doesNotMatch(h.element('responseBody').textContent,/old data/);
});

test('App 日志不进入实时请求列表，旧记录详情仍不伪造请求响应',async()=>{
 const h=setup();h.state.https={enabled:true,domains:['fat-api.example.com']};h.state.event_details=[{id:7,text:'09:41:20 [phone] [HTTP][App 日志] https://fat-api.example.live/track · com.example.app',host:'fat-api.example.live',app:'com.example.app',serial:'A',scope:'log',status:'unknown'}];h.state.events=h.state.event_details.map(e=>e.text);
 h.run('renderEvents()');assert.doesNotMatch(h.element('events').innerHTML,/data-request-detail="7"/);assert.equal(h.run('filteredEvents().length'),0);
 await h.run('openRequestDetail(7)');assert.equal(h.calls.length,0);assert.match(h.element('requestCaptureNote').textContent,/App 日志/);assert.match(h.element('requestEnableHTTPS').textContent,/fat-api.example.live/);assert.equal(h.element('requestEnableHTTPS').hidden,false);assert.equal(h.element('responseBody').textContent,'未采集');assert.equal(h.element('copyResponse').disabled,true);
 h.state.https.domains=['example.live'];h.run('renderUncapturedDetail(requestDetailEvent)');assert.equal(h.element('requestEnableHTTPS').hidden,true);assert.match(h.element('requestCaptureNote').textContent,/重新发起请求/);
});
test('隧道详情保持连接状态，过期解密详情明确不可用，不遗留前一请求正文',async()=>{
 const h=setup();h.state.event_details=[{id:8,text:'09:00:00 [phone] [HTTPS 隧道][代理] CONNECT api.example:443',host:'api.example',scope:'tunnel',status:'active'},{id:9,text:'09:01:00 [phone] [HTTP][代理] GET https://api.example/test',host:'api.example',scope:'http',status:'complete',decrypted:true}];
 await h.run('openRequestDetail(8)');assert.match(h.element('requestDetailTitle').textContent,/连接中/);assert.match(h.element('requestCaptureNote').textContent,/隧道/);
 h.setHandler(()=>({ok:false,text:async()=>'详情已过期'}));h.element('responseBody').textContent='old sensitive body';await h.run('openRequestDetail(9)');assert.equal(h.element('responseBody').textContent,'未采集');assert.match(h.element('requestCaptureNote').textContent,/已过期/);
});

test('请求行速率只匹配同手机 App 域名，未采样显示未知并保留请求详情入口',()=>{
 const h=setup();
 const event={id:7,text:'10:00:00 [phone A] [HTTP][代理] GET https://API.EXAMPLE./one · com.example.app',status:'complete',scope:'http',serial:'A',app:'com.example.app',host:'API.EXAMPLE.'};
 h.state.event_details=[event];h.state.events=[event.text];
 h.state.lanes=[{serial:'A',stats:{hosts:[{app:'com.example.app',host:'api.example',kbs:1.2,queued_kb:3},{app:'com.example.app',host:'api.example',kbs:0.8,queued_kb:2},{app:'com.other.app',host:'api.example',kbs:90,queued_kb:90},{app:'com.example.app',host:'other.example',kbs:80,queued_kb:80}]}},{serial:'B',stats:{hosts:[{app:'com.example.app',host:'api.example',kbs:70,queued_kb:70}]}}];
 h.run('renderEvents()');let output=h.element('events').innerHTML;
 assert.match(output,/>2\.0 KB\/s<\/td>/);assert.equal(h.run('liveTrafficIndex().values().next().value.queue'),5);assert.match(output,/不是单条请求吞吐/);assert.match(output,/data-request-detail="7"/);
 h.state.lanes=[];h.run('renderEvents()');output=h.element('events').innerHTML;
 assert.equal(h.run('liveTrafficIndex().size'),0);assert.match(output,/<td class="metric"[^>]*>—<\/td>/);assert.doesNotMatch(output,/>0\.0 KB\/s<\/td>/);
});

test('状态下拉筛选请求中接口，完成后自动移出，不包含隧道与 App 日志',()=>{
 const h=setup(),make=(id,serial,scope,status)=>({id,serial,scope,status,app:'com.example.app',host:'api.example',text:`10:00:00 [phone] [HTTP][代理] GET https://api.example/${id} · com.example.app`});
 h.state.event_details=[make(1,'A','http','active'),make(2,'B','http','active'),make(3,'A','tunnel','active'),make(4,'A','log','unknown'),make(5,'A','http','complete')];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("fSerial='A';$('evFollow').checked=true;renderEvents()");
 h.element('evStatus').value='request_active';h.run('renderEvents()');assert.equal(h.run('filteredEvents().length'),1);assert.equal(h.run('filteredEvents()[0].id'),1);
 h.state.event_details[0].status='complete';h.run('renderEvents()');assert.match(h.element('events').innerHTML,/当前没有正在请求的接口/);
 h.element('evStatus').value='tunnel_active';assert.equal(h.run('filteredEvents().length'),1);assert.equal(h.run('filteredEvents()[0].id'),3);
 h.element('evStatus').value='';h.run('renderEvents()');assert.equal(h.run('filteredEvents().length'),3);
});

test('请求时间显示开始与响应结束及持续耗时，日志不推断响应时间',()=>{
 const h=setup();h.run('const timingStart=1700000000123');
 const complete=h.run("eventTimingText({scope:'http',status:'complete',started:timingStart,ended:timingStart+333})");
 assert.match(complete,/请求开始 .*\.123/);assert.match(complete,/响应结束 .*\.456/);assert.match(complete,/耗时 333 ms/);
 const active=h.run("eventTimingText({scope:'http',status:'active',started:timingStart,duration_ms:2500})");assert.match(active,/已进行 2\.5 s/);assert.match(active,/—/);
 const log=h.run("eventTimingText({scope:'log',status:'unknown',recorded_at:timingStart})");assert.match(log,/记录 .*\.123/);assert.match(log,/响应结束 — · 耗时 —/);assert.doesNotMatch(log,/请求开始/);
 const tunnel=h.run("eventTimingText({scope:'tunnel',status:'complete',started:timingStart,ended:timingStart+50})");assert.match(tunnel,/连接开始/);assert.match(tunnel,/连接结束/);assert.doesNotMatch(tunnel,/响应结束/);
 h.state.event_details=[{id:1,scope:'http',status:'complete',text:'10:00:00 [phone] [HTTP][代理] GET https://api.example',started:1700000000123,ended:1700000000456}];h.state.events=h.state.event_details.map(e=>e.text);h.run('renderEvents()');assert.match(h.element('events').innerHTML,/title="请求开始[^"]*响应结束[^"]*耗时 333 ms"/);assert.match(h.run('eventMenuContent(filteredEvents()[0])'),/耗时 333 ms/);
});

test('操作菜单打开时固定请求行，关闭后再显示新增请求以避免操作对象错位',()=>{
 const h=setup(),old={id:1,text:'12:00:00 [phone] [HTTP][代理] GET https://old.example',host:'old.example',status:'active',scope:'http'};
 h.state.event_details=[old];h.state.events=[old.text];h.run('renderEvents();eventMenuOpen=true');
 const before=h.element('events').innerHTML;
 h.state.event_details=[{...old,id:2,text:old.text.replace('old.example','new.example'),host:'new.example'},old];h.state.events=h.state.event_details.map(e=>e.text);h.run('renderEvents()');
 assert.equal(h.element('events').innerHTML,before);assert.equal(h.run('eventMenuRows[0].host'),'old.example');
 h.run('eventMenuOpen=false;renderEvents()');assert.match(h.element('events').innerHTML,/new.example/);assert.equal(h.run('eventMenuRows[0].host'),'new.example');
});

test('实时请求始终排除 App 日志和工具事件，计数与协议筛选仅包含网络记录',()=>{
 const h=setup();h.state.event_details=[
  {id:1,scope:'http',status:'active',serial:'A',text:'12:00:00 [phone] [HTTP][代理] GET http://api.example/data.json'},
  {id:2,scope:'tunnel',status:'active',serial:'A',host:'api.example',text:'12:00:00 [phone] [HTTPS 隧道][代理] CONNECT api.example:443'},
  {id:3,scope:'log',status:'unknown',serial:'A',text:'12:00:00 [phone] [HTTP][App 日志] GET http://api.example/data.json'},
  {id:4,text:'12:00:00 tool event'}
 ];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("$('evFollow').checked=false;renderEvents()");assert.equal(h.run('filteredEvents().map(e=>e.id).join()'),'1,2');assert.match(h.element('evCount').textContent,/显示 2 \/ 2 条/);assert.equal(h.state.event_details.length,4);
 h.run("requestProtocol='https';renderEvents()");assert.equal(h.run('filteredEvents().map(e=>e.id).join()'),'2');
 h.run("requestProtocol='';requestContentType='json';renderEvents()");assert.equal(h.run('filteredEvents().map(e=>e.id).join()'),'1');
 h.state.event_details=undefined;h.run("requestContentType='';renderEvents()");assert.equal(h.run('filteredEvents().length'),2);assert.doesNotMatch(h.element('events').innerHTML,/App 日志|tool event/);
});

test('表格序号对应完整请求列表，筛选与排序不重排编号，详情仍绑定原请求 ID',()=>{
 const h=setup(),make=(id,path)=>({id,scope:'http',status:'complete',text:`12:00:00 [phone] [HTTP][代理] GET https://api.example/${path}`});
 h.state.event_details=[make(192,'new'),{...make(180,'log'),scope:'log'},make(172,'middle'),make(20,'old')];h.state.events=h.state.event_details.map(e=>e.text);
 const numbers=()=>[...h.element('events').innerHTML.matchAll(/<td class="row-id"[^>]*>(\d+)<\/td>/g)].map(m=>Number(m[1]));
 h.run('renderEvents()');assert.deepEqual(numbers(),[3,2,1]);assert.match(h.element('events').innerHTML,/title="请求 ID 192">3/);assert.match(h.element('events').innerHTML,/data-request-detail="192"/);
 h.element('evSearch').value='middle';h.run('requestSelected=172;renderEvents()');assert.deepEqual(numbers(),[2]);assert.match(h.element('events').innerHTML,/data-request-row="172" aria-selected="true"/);assert.match(h.element('events').innerHTML,/data-request-detail="172"/);
 h.element('evSearch').value='';h.run('requestSortDesc=false;renderEvents()');assert.deepEqual(numbers(),[1,2,3]);assert.match(h.element('events').innerHTML,/title="请求 ID 20">1/);
});

test('历史读取请求记录，复用实时列表全部列、正文计数与详情 ID',async()=>{
 const h=setup(),row={id:900,serial:'A',device:'Phone offline',app:'com.example',host:'api.example',scope:'http',status:'complete',decrypted:true,method:'POST',url:'https://api.example/items?token=REDACTED',text:'12:00:00 [Phone] [HTTP][代理] POST https://api.example/items?token=REDACTED · com.example',status_code:201,content_type:'application/json',started:1700000000123,ended:1700000000456,duration_ms:333,request_bytes:25,response_bytes:512};
 h.state.event_details=[row];h.state.events=[row.text];h.run("view='history';renderEvents()");
 h.setHandler(()=>h.response({rows:[row],summary:{count:1,active:0,failed:0,up:25,down:512,avg_ms:333},next_before:0}));await h.run('refreshConns(true)');
 assert.match(h.calls[0].url,/^\/api\/request-history\?/);assert.match(h.element('exportCsv').href,/^\/api\/request-history.csv/);
 const live=h.element('events').innerHTML,history=h.element('conns').innerHTML;
 assert.equal(history.replaceAll('实时请求列表 历史','流量与请求列表').replaceAll('historyRequestHelp','requestListHelp').replaceAll('data-history-sort','data-request-sort').replaceAll('data-history-more','data-event-more'),live);
 assert.match(history,/data-request-detail="900"/);assert.match(history,/Phone offline/);assert.match(history,/25 B/);assert.match(history,/512 B/);assert.match(history,/已解密/);assert.doesNotMatch(history,/data-detail=/);
});
test('离开实时列表的历史请求仍按原 ID 显示摘要，详情过期不冒用当前请求',async()=>{
 const h=setup();h.run("view='history';connData={rows:[{id:700,scope:'http',decrypted:true,host:'old.example',url:'https://old.example/items',text:'12:00:00 [phone] [HTTP][代理] GET https://old.example/items',status:'complete',status_code:200}],summary:{count:1}};");
 h.setHandler(()=>({ok:false,text:async()=>'请求详情已过期或未采集'}));await h.run('openRequestDetail(700)');
 assert.equal(h.run('requestDetailEvent.id'),700);assert.match(h.element('requestDetailURL').textContent,/old.example/);assert.match(h.element('requestCaptureNote').textContent,/详情暂不可用/);
});
test('历史分页保留游标并传递请求类型、关键词与状态，复制当前排序后的请求',async()=>{
 const h=setup();h.run("view='history';let historyCopied='';navigator.clipboard.writeText=async text=>historyCopied=text");
 h.element('hSearch').value='REDACTED';h.element('hType').value='decrypted';h.element('hStatus').value='errors';
 const row={id:800,scope:'http',text:'12:00:00 [Phone] [HTTP][代理] GET https://api.example/error?token=REDACTED',status:'timeout',detail:'i/o timeout'};
 h.setHandler(()=>h.response({rows:[row],summary:{count:101,active:0,failed:101,up:0,down:0,avg_ms:2},next_before:800}));await h.run('refreshConns(true)');h.run("$('hNext').onclick()");await new Promise(resolve=>setImmediate(resolve));
 assert.match(h.calls[1].url,/before=800/);assert.match(h.calls[1].url,/type=decrypted/);assert.match(h.calls[1].url,/q=REDACTED/);assert.match(h.calls[1].url,/status=errors/);assert.match(h.element('hPage').textContent,/第 2 页.*自动更新已暂停/);
 await h.run("$('copyConns').onclick()");assert.match(h.run('historyCopied'),/api.example\/error\?token=REDACTED/);assert.match(h.run('historyCopied'),/请求失败（超时）/);
});
test('历史菜单打开时冻结表格，菜单使用历史行而非实时行',()=>{
 const h=setup();h.run("connData={rows:[{id:700,scope:'http',text:'12:00:00 old history',status:'complete'}],summary:{count:1,active:0,failed:0,up:0,down:0,avg_ms:0}};renderConns()");
 const original=h.element('conns').innerHTML;h.run("eventMenuOpen=true;connData.rows=[{id:900,scope:'http',text:'12:00:01 new history'}];renderConns()");
 assert.equal(h.element('conns').innerHTML,original);assert.equal(h.run('historyMenuRows[0].id'),700);
 h.run("eventMenuOpen=false;renderConns()");assert.match(h.element('conns').innerHTML,/data-request-detail="900"/);
});

test('暂停列表固定记录、状态、速率和滚动位置，后台轮询继续',async()=>{
 const h=setup(),old={id:1,scope:'http',serial:'A',app:'com.example',host:'api.example',url:'https://api.example/old',text:'12:00:00 [phone] [HTTP][代理] GET https://api.example/old',status:'active',duration_ms:10};
 h.state.event_details=[old];h.state.lanes=[{serial:'A',stats:{hosts:[{host:'api.example',app:'com.example',kbs:2}]}}];h.run("fSerial='A';renderEvents();$('pauseRequests').onclick()");
 h.element('events').scrollTop=250;h.element('events').scrollLeft=60;const before=h.element('events').innerHTML,count=h.element('evCount').textContent;
 old.status='complete';h.state.lanes[0].stats.hosts[0].kbs=99;
 const incoming={...h.state,event_seq:5,event_details:[{...old,id:2,url:'https://api.example/new',text:old.text.replace('old','new')},old]};
 h.setHandler(()=>h.response(incoming));h.run("fSerial='B'");await h.run('refresh()');h.run('renderEvents()');
 assert.equal(h.run('state.event_seq'),5);assert.equal(h.run('displayState.event_details.length'),2);assert.equal(h.element('events').innerHTML,before);assert.equal(h.element('evCount').textContent,count);
 assert.equal(h.element('events').scrollTop,250);assert.equal(h.element('events').scrollLeft,60);assert.match(count,/列表已暂停/);assert.match(before,/2.0 KB\/s/);assert.ok(h.calls.every(c=>!c.options.body));assert.equal(h.element('pauseRequests').textContent,'恢复列表');
});
test('重新实时跟随显示后台最新记录，恢复最新在前并定位最新请求',()=>{
 const h=setup(),old={id:1,scope:'http',text:'12:00:00 old',status:'active'};
 h.state.event_details=[old];h.run("renderEvents();$('evFollow').checked=false;$('evFollow').onchange()");h.element('events').scrollTop=300;h.element('events').scrollLeft=80;
 h.state.event_details=[{...old,id:9,text:'12:00:09 newest'},old];h.run("requestSort='url';requestSortDesc=false;$('evFollow').checked=true;$('evFollow').onchange()");
 assert.equal(h.run('requestSort'),'id');assert.equal(h.run('requestSortDesc'),true);assert.equal(h.run('requestSelected'),9);assert.equal(h.run('eventMenuRows[0].id'),9);assert.equal(h.element('events').scrollTop,0);assert.equal(h.element('events').scrollLeft,80);assert.match(h.element('events').innerHTML,/data-request-row="9" aria-selected="true"/);
 h.element('events').scrollTop=150;h.run('renderEvents()');assert.equal(h.element('events').scrollTop,0);assert.equal(h.calls.length,0);
});
test('清空只影响当前列表，历史和后台请求保留，后续新请求照常显示',()=>{
 const h=setup(),old={id:1,recorded_at:Date.now()-1000,scope:'http',text:'12:00:00 old',status:'complete'};h.state.event_details=[old];
 h.run("connData={rows:state.event_details.slice(),summary:{count:1}};renderEvents();$('clearRequests').onclick()");
 assert.equal(h.run('filteredEvents().length'),0);assert.equal(h.run('state.event_details.length'),1);assert.equal(h.run('connData.rows.length'),1);assert.equal(h.calls.length,0);assert.ok(h.storage.has('wn_request_list_cleared_at'));assert.match(h.element('events').innerHTML,/没有符合条件的请求/);
 const time=h.run('requestListClearedAt+1');h.state.event_details=[{...old,id:2,recorded_at:time,text:'12:00:01 new'},old];h.run('renderEvents()');assert.equal(h.run('eventMenuRows.map(e=>e.id).join()'),'2');
 h.run("requestListClearedAt=Number(store.get('request_list_cleared_at'));renderEvents()");assert.equal(h.run('eventMenuRows.map(e=>e.id).join()'),'2');
});
test('暂停列表时清空仍保持冻结，恢复后才显示新采集请求',()=>{
 const h=setup(),old={id:1,recorded_at:Date.now()-1000,scope:'http',text:'12:00:00 old',status:'complete'};h.state.event_details=[old];
 h.run("renderEvents();$('pauseRequests').onclick();$('clearRequests').onclick()");
 h.state.event_details=[{...old,id:3,recorded_at:h.run('requestListClearedAt+1'),text:'12:00:01 new'}];h.run('renderEvents()');assert.equal(h.run('eventMenuRows.length'),0);
 h.run("$('pauseRequests').onclick()");assert.equal(h.run('eventMenuRows[0].id'),3);assert.equal(h.run('requestSelected'),3);assert.equal(h.calls.length,1);assert.equal(h.element('pauseRequests').textContent,'暂停列表');
});
test('菜单关闭后完成实时跟随定位，全局暂停仍允许停留在当前滚动位置',()=>{
 const h=setup();h.state.event_details=[{id:7,scope:'http',text:'12:00:00 newest',status:'active'}];h.run("eventMenuOpen=true;$('evFollow').checked=true;$('evFollow').onchange()");
 assert.equal(h.run('requestFollowJumpPending'),true);h.element('events').scrollTop=200;h.run('eventMenuOpen=false;renderEvents()');assert.equal(h.element('events').scrollTop,0);assert.equal(h.run('requestSelected'),7);
 h.run('paused=true');h.element('events').scrollTop=120;h.run('renderEvents()');assert.equal(h.element('events').scrollTop,120);assert.equal(h.run('paused'),true);
});

test('152 条请求中筛选出的第 151 条仍显示 151，与事件 ID 的间隔无关',()=>{
 const h=setup();h.state.event_details=Array.from({length:152},(_,i)=>({id:(i+1)*7,scope:'http',serial:'A',status:i===150?'active':'complete',method:'GET',url:`https://api.example/${i+1}.json`,text:`12:00:00 [phone] [HTTP][代理] GET https://api.example/${i+1}.json`})).reverse();
 h.state.event_details.splice(5,0,{id:1100,scope:'log',text:'12:00:00 [App 日志] invisible'});h.element('evStatus').value='active';h.run('renderEvents()');
 assert.match(h.element('evCount').textContent,/显示 1 \/ 152 条/);assert.match(h.element('events').innerHTML,/title="请求 ID 1057">151<\/td>/);assert.match(h.element('events').innerHTML,/data-request-detail="1057"/);
 h.run("requestSort='url';requestSortDesc=false;renderEvents()");assert.match(h.element('events').innerHTML,/title="请求 ID 1057">151<\/td>/);
});

test('资源预览识别 MP4、SVGA、图片和文本，POST 与加密隧道不重放',()=>{
 const h=setup();for(const [url,type,expected] of [['https://files.example/a.MP4?x=REDACTED','application/octet-stream','video'],['https://files.example/a.svga','application/zip','svga'],['https://files.example/a.txt','','text'],['https://files.example/a','application/json','text'],['https://files.example/a.webp','','image']]) {
  assert.equal(h.run(`resourceKind(${JSON.stringify({method:'GET',scope:'http',url,content_type:type})})`),expected);
 }
 assert.equal(h.run("resourceKind({method:'POST',scope:'http',url:'https://files.example/a.mp4'})"),'');assert.equal(h.run("resourceKind({method:'CONNECT',scope:'tunnel',url:'https://files.example:443'})"),'');
});
test('视频预览只使用请求 ID 地址，轮询不重置播放器，关闭后释放播放与加载',async()=>{
 const h=setup();h.setHandler(()=>h.response({kind:'video',resource_url:'/api/requests/7/resource'}));
 await h.run("showResourcePreview({id:7,method:'GET',scope:'http',url:'https://files.example/a.mp4?signature=REDACTED',preview_available:true})");
 const media=h.element('resourcePreviewHost').children[0];assert.equal(media.tagName,'VIDEO');assert.equal(media.controls,true);assert.equal(media.src,'/api/requests/7/resource');assert.equal(h.element('resourcePreview').hidden,false);
 await h.run("showResourcePreview({id:7,method:'GET',scope:'http',url:'https://files.example/a.mp4',preview_available:true})");assert.equal(h.calls.length,1);assert.equal(h.element('resourcePreviewHost').children[0],media);
 h.run('stopResourcePreview()');assert.equal(media.paused,true);assert.equal(media.loaded,true);assert.equal(h.element('resourcePreviewHost').children.length,0);assert.equal(h.element('resourcePreview').hidden,true);
});
test('切换资源时取消旧文本加载，较晚返回的资源不会覆盖新图片',async()=>{
 const h=setup();let finish;h.setHandler(url=>url==='/api/requests/1/preview'?h.response({kind:'text',resource_url:'/api/requests/1/resource'}):url==='/api/requests/2/preview'?h.response({kind:'image',resource_url:'/api/requests/2/resource'}):{ok:true,arrayBuffer:()=>new Promise(resolve=>finish=resolve)});
 const first=h.run("showResourcePreview({id:1,method:'GET',url:'https://files.example/a.txt',preview_available:true})");await new Promise(resolve=>setImmediate(resolve));
 const request=h.calls.find(c=>c.url==='/api/requests/1/resource');await h.run("showResourcePreview({id:2,method:'GET',url:'https://files.example/a.png',preview_available:true})");assert.equal(request.options.signal.aborted,true);
 finish(new TextEncoder().encode('<script>unsafe()</script>').buffer);await first;
 assert.equal(h.element('resourcePreviewHost').children[0].tagName,'IMG');assert.equal(h.element('resourcePreviewHost').children[0].src,'/api/requests/2/resource');assert.equal(h.element('resourcePreviewHost').children.length,1);
});
test('文本以纯文本展示，旧记录使用已采集片段而不猜测脱敏地址',async()=>{
 const h=setup();await h.run("showResourcePreview({id:1,method:'GET',url:'https://files.example/a.html'},{response:{body:'<script>unsafe()</script>',truncated:true,headers:{}}})");
 assert.equal(h.element('resourcePreviewHost').children[0].tagName,'PRE');assert.equal(h.element('resourcePreviewHost').children[0].textContent,'<script>unsafe()</script>');assert.match(h.element('resourcePreviewStatus').textContent,/已截断/);assert.equal(h.calls.length,0);
});
test('清除数据调用服务端清空接口，并重置实时列表的本地清空标记',async()=>{
 const h=setup(),old={id:1,recorded_at:Date.now()-1000,scope:'http',text:'12:00:00 old',status:'complete'};h.state.event_details=[old];
 h.run("renderEvents();$('clearRequests').onclick()");assert.ok(Number(h.run('requestListClearedAt'))>0);
 h.setHandler(url=>h.response(url.startsWith('/api/request-history/clear')?{ok:true}:{...h.state,event_details:[]}));
 h.run('ask=async()=>true');await h.run("$('clearData').onclick()");
 assert.ok(h.calls.some(c=>c.url==='/api/request-history/clear'&&c.options.method==='POST'));
 assert.equal(h.run('requestListClearedAt'),0);assert.equal(h.run('filteredEvents().length'),0);
});
test('实时列表分批渲染，避免一次渲染上万行',()=>{
 const h=setup(),now=Date.now();h.state.event_details=Array.from({length:1200},(_,i)=>({id:1200-i,recorded_at:now,scope:'http',text:'12:00:00 r'+i,status:'complete'}));
 h.run('renderEvents()');assert.equal(h.run('eventMenuRows.length'),1200);assert.equal((h.element('events').innerHTML.match(/data-request-row=/g)||[]).length,500);
 assert.match(h.element('events').innerHTML,/还有 700 条未渲染/);assert.match(h.element('events').innerHTML,/<td[^>]*>\s*<\/td>|1200/);
 h.run('requestRenderLimit+=requestRenderStep;renderEvents()');assert.equal((h.element('events').innerHTML.match(/data-request-row=/g)||[]).length,1000);
});
test('点击 SSL 列的「加密」可添加该域名并开启解密，已在解密列表的域名不再可点',async()=>{
 const h=setup();h.state.https={enabled:false,domains:[]};
 assert.match(h.run("sslUnlock({host:'api.example.com'})"),/data-https="api.example.com"/);
 h.state.https={enabled:true,domains:['example.com']};
 assert.equal(h.run("sslUnlock({host:'api.example.com'})"),'加密');
 assert.equal(h.run("sslUnlock({})"),'加密');
});
test('设备操作提供 USB 与局域网两种启用方式，局域网设备显示代理地址',()=>{
 const h=setup();h.state.devices=[{serial:'A',model:'phone A',state:'device',enabled:false}];
 h.run("toolSerials=new Set(['A']);toolSelectionReady=true;renderClear()");
 const actions=h.element('toolDeviceActions').innerHTML;assert.match(actions,/data-tool-action="on"/);assert.match(actions,/data-tool-action="on-lan"/);
 h.state.adb=true;h.state.devices=[{serial:'A',model:'phone A',state:'device',enabled:true,port:8888,lan:'192.168.1.2:8888'}];
 h.run('renderDevices()');assert.match(h.element('devices').innerHTML,/局域网 192\.168\.1\.2:8888/);
});
test('解密域名输入框一次添加多个域名，去重并沿用已有名单',async()=>{
 const h=setup();h.state.https={enabled:false,domains:['old.example'],fingerprint:'ABCD',expires:'2036-10-12',directory:'/local/weaknet'};h.run('renderHTTPS()');
 h.setHandler((url,options)=>h.response({...h.state.https,...JSON.parse(options.body)}));
 assert.equal(await h.run("addHTTPSDomain('API.example, cdn.example；old.example\\nimg.example.')"),true);
 assert.deepEqual(JSON.parse(h.calls[0].options.body).domains,['old.example','api.example','cdn.example','img.example']);
 assert.equal(h.element('httpsDraftActions').hidden,true);
 h.setHandler(()=>({ok:false,text:async()=>JSON.stringify({error:'disk full'})}));assert.equal(await h.run("addHTTPSDomain('new.example')"),false);
 assert.equal(h.element('httpsDraftActions').hidden,false);
});

test('工具事件弹窗只列出工具和系统事件，可搜索、复制并标出失败',async()=>{
 const h=setup();
 h.state.event_details=[{id:4,text:'12:00:03 [SM G991U] 清空 com.example.app 失败：denied'},{id:3,text:'12:00:02 [HTTP][代理] GET http://api.example.com/a',scope:'http',status:'complete'},{id:2,text:'12:00:01 [SM G991U] [App 日志] https://api.example.com/b',scope:'log'},{id:1,text:'12:00:00 [SM G991U] 已启用弱网，代理 127.0.0.1:8899'}];
 h.run("$('toolEventsOpen').onclick()");
 assert.equal(h.run('toolEvents().length'),2);assert.equal(h.element('toolEventsCount').textContent,'共 2 条');
 const list=h.element('toolEventsList').innerHTML;assert.match(list,/<time>12:00:03<\/time><span class="bad">\[SM G991U\] 清空 com.example.app 失败/);assert.match(list,/<time>12:00:00<\/time><span>\[SM G991U\] 已启用弱网/);assert.doesNotMatch(list,/api\.example\.com/);
 h.element('toolEventsSearch').value='弱网';h.run("$('toolEventsSearch').oninput()");assert.equal(h.element('toolEventsCount').textContent,'显示 1 / 2 条');
 let copied='';h.run('navigator.clipboard').writeText=async text=>{copied=text};await h.run("$('copyToolEvents').onclick()");assert.equal(copied,'12:00:00 [SM G991U] 已启用弱网，代理 127.0.0.1:8899');
});
