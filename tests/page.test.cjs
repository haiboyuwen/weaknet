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

test('事件按结构化手机、App 和域名跟随观测范围，复制保留原文，独立筛选显示包名',async()=>{
 const h=setup();const row={text:'12:00:00 [Same model] [HTTP][代理] GET http://api.example.com/a · com.example.app · 弱网',serial:'A',app:'com.example.app',host:'api.example.com',status:'active',scope:'http'};
 h.state.event_details=[row,{...row,serial:'B'},{...row,app:'com.other.app'},{...row,host:'cdn.example.com'},{text:'12:00:00 tool event'}];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("fSerial='A';fApp='com.example.app';fHost='api.';renderEvents()");assert.equal(h.run('filteredEvents().length'),1);
 assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/com.example.app/);assert.doesNotMatch(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 const menu=h.run('eventMenuContent(filteredEvents()[0])');assert.match(menu,/data-copy=/);assert.match(menu,/com.example.app/);assert.match(menu,/请求中/);
 h.element('evFollow').checked=false;h.run("$('evFollow').onchange()");assert.equal(h.run('filteredEvents().length'),4);assert.match(h.run('eventContent(filteredEvents()[0])'),/data-app="com.example.app"/);assert.match(h.run('eventContent(filteredEvents()[0])'),/Same model/);
 h.element('evFollow').checked=true;h.run("fSerial='';fApp='';fHost='';renderEvents()");assert.equal(h.run('filteredEvents().length'),4);
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

test('请求中快捷筛选只显示当前范围内未完成接口，完成后自动移出并更新计数',()=>{
 const h=setup(),make=(id,serial,scope,status)=>({id,serial,scope,status,app:'com.example.app',host:'api.example',text:`10:00:00 [phone] [HTTP][代理] GET https://api.example/${id} · com.example.app`});
 h.state.event_details=[make(1,'A','http','active'),make(2,'B','http','active'),make(3,'A','tunnel','active'),make(4,'A','log','unknown'),make(5,'A','http','complete')];h.state.events=h.state.event_details.map(e=>e.text);
 h.run("fSerial='A';$('evFollow').checked=true;renderEvents()");assert.equal(h.element('evPending').textContent,'请求中 1');
 h.run("$('evPending').onclick()");assert.equal(h.element('evStatus').value,'request_active');assert.equal(h.run('filteredEvents().length'),1);assert.equal(h.run('filteredEvents()[0].id'),1);
 h.state.event_details[0].status='complete';h.run('renderEvents()');assert.equal(h.element('evPending').textContent,'请求中 0');assert.match(h.element('events').innerHTML,/当前没有正在请求的接口/);
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

test('表格序号在隐藏日志、筛选和排序后连续，详情与选中仍绑定原请求 ID',()=>{
 const h=setup(),make=(id,path)=>({id,scope:'http',status:'complete',text:`12:00:00 [phone] [HTTP][代理] GET https://api.example/${path}`});
 h.state.event_details=[make(192,'new'),{...make(180,'log'),scope:'log'},make(172,'middle'),make(20,'old')];h.state.events=h.state.event_details.map(e=>e.text);
 const numbers=()=>[...h.element('events').innerHTML.matchAll(/<td class="row-id"[^>]*>(\d+)<\/td>/g)].map(m=>Number(m[1]));
 h.run('renderEvents()');assert.deepEqual(numbers(),[3,2,1]);assert.match(h.element('events').innerHTML,/title="请求 ID 192">3/);assert.match(h.element('events').innerHTML,/data-request-detail="192"/);
 h.element('evSearch').value='middle';h.run('requestSelected=172;renderEvents()');assert.deepEqual(numbers(),[1]);assert.match(h.element('events').innerHTML,/data-request-row="172" aria-selected="true"/);assert.match(h.element('events').innerHTML,/data-request-detail="172"/);
 h.element('evSearch').value='';h.run('requestSortDesc=false;renderEvents()');assert.deepEqual(numbers(),[1,2,3]);assert.match(h.element('events').innerHTML,/title="请求 ID 20">1/);
});
