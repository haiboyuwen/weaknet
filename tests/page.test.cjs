const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const page=fs.readFileSync(require('node:path').join(__dirname,'../page.html'),'utf8');
const script=page.split('<script>')[1].split('</script>')[0].split('\nrenderProfiles();selectView(view);refresh();')[0];
function setup(){
 const elements=new Map(),storage=new Map(),calls=[],timers=new Map();let clock=0;
 function element(id){if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',style:{},firstChild:{textContent:''},classList:{toggle(){}},clientWidth:0,hidden:false,disabled:false,setAttribute(){},removeAttribute(){},addEventListener(){},contains(){return false},showModal(){},close(){},focus(){},select(){},appendChild(node){node.parentElement=this;return node}});return elements.get(id)}
 const config={down_kbps:400,up_kbps:200,delay_ms:200,jitter_ms:50,loss_pct:1,window_kb:128,outage:'',blacklist_on:false,blacklist:[],whitelist:[]};
 const state={config,presets:[],devices:[],lanes:[],apps:[],events:[],event_seq:0,adb:false,_appFilter:''};
 const response=data=>({ok:true,text:async()=>JSON.stringify(data)});
 let handler=(url,options)=>response(url.startsWith('/api/config')?{...config,...JSON.parse(options.body)}:state);
 const context=vm.createContext({
  document:{getElementById:element,querySelectorAll:()=>[],activeElement:null,hidden:false,addEventListener(){}},
  window:{getSelection:()=>null,addEventListener(){}},navigator:{clipboard:{writeText:async()=>{}}},
  localStorage:{getItem:key=>storage.get(key),setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
  setTimeout:fn=>{timers.set(++clock,fn);return clock},clearTimeout:key=>timers.delete(key),setInterval(){},
  fetch:async(url,options)=>{calls.push({url,options});return handler(url,options)},
  AbortController,URLSearchParams,URL,Blob,Date,devicePixelRatio:1,confirm:()=>true,prompt:()=>null
 });
 const run=code=>vm.runInContext(code,context);run(script);context.testState=state;run('state=testState;displayState=state;renderConfig()');
 return {run,element,storage,calls,state,response,setHandler:fn=>handler=fn};
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
 const h=setup();h.state.event_details=[{text:'[HTTP][代理] GET http://example.com',status:'active',scope:'http'},{text:'[HTTP][代理] GET http://example.com/timeout',status:'timeout',scope:'http',detail:'i/o timeout'},{text:'[HTTPS 隧道][代理] CONNECT example.com:443',status:'complete',scope:'tunnel'},{text:'[资源][App 日志] https://example.com/a.mp4',status:'unknown',scope:'log'}];
 h.state.events=h.state.event_details.map(e=>e.text);h.run('renderEvents()');assert.match(h.element('events').innerHTML,/请求中/);assert.match(h.element('events').innerHTML,/隧道已结束/);assert.match(h.element('events').innerHTML,/结果未知/);
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

test('事件按结构化手机、App 和域名跟随观测范围，复制保留原文，独立筛选显示包名',async()=>{
 const h=setup();const row={text:'12:00:00 [Same model] [HTTP][代理] GET http://api.example.com/a · com.example.app · 弱网',serial:'A',app:'com.example.app',host:'api.example.com',status:'active',scope:'http'};
 h.state.event_details=[row,{...row,serial:'B'},{...row,app:'com.other.app'},{...row,host:'cdn.example.com'},{text:'12:00:00 tool event'}];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("fSerial='A';fApp='com.example.app';fHost='api.';renderEvents()");assert.equal(h.run('filteredEvents().length'),1);
 assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/com.example.app/);assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 h.run("let copiedEvent='';navigator.clipboard.writeText=async text=>{copiedEvent=text}");await h.run("$('copyEvents').onclick()");assert.match(h.run('copiedEvent'),/com.example.app/);assert.match(h.run('copiedEvent'),/请求中/);
 h.element('evFollow').checked=false;h.run("$('evFollow').onchange()");assert.equal(h.run('filteredEvents().length'),5);assert.match(h.run('eventContent(filteredEvents()[0])'),/data-app="com.example.app"/);assert.match(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 h.element('evFollow').checked=true;h.run("fSerial='';fApp='';fHost='';renderEvents()");assert.equal(h.run('filteredEvents().length'),5);
});
