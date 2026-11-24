package api

import "html/template"

// opsTemplate is intentionally minimal and inline: no build step, no static
// assets beyond one inline script, no CDN. It polls /ops/data every 2s, which is
// simpler and more robust than SSE for a page one operator opens for a minute.
//
// Colours follow the UX-SPEC design tokens.
var opsTemplate = template.Must(template.New("ops").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>CoDraw · 运维面板</title>
<style>
  :root{--paper:#F7F6F2;--ink:#16181D;--ink-soft:#5B6070;--line:#E4E2DA;--brand:#3D5AFE;--ok:#12B886;--warn:#E8590C;--panel:#fff}
  *{box-sizing:border-box}
  body{margin:0;background:var(--paper);color:var(--ink);font:14px/1.5 ui-sans-serif,"PingFang SC","Microsoft YaHei",system-ui,sans-serif}
  header{display:flex;align-items:center;gap:12px;padding:14px 20px;background:var(--panel);border-bottom:1px solid var(--line);flex-wrap:wrap}
  header h1{font-size:15px;margin:0;font-weight:600}
  .badge{font:12px ui-monospace,"SF Mono",monospace;color:var(--ink-soft);border:1px solid var(--line);border-radius:999px;padding:2px 8px}
  .dot{width:8px;height:8px;border-radius:50%;background:var(--ok);display:inline-block}
  main{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:16px;padding:20px}
  section{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:14px 16px}
  h2{font-size:12px;text-transform:uppercase;letter-spacing:.08em;color:var(--ink-soft);margin:0 0 10px}
  dl{display:grid;grid-template-columns:1fr auto;gap:6px 12px;margin:0}
  dt{color:var(--ink-soft)}dd{margin:0;font:13px ui-monospace,"SF Mono",monospace;text-align:right}
  table{width:100%;border-collapse:collapse;font:13px ui-monospace,"SF Mono",monospace}
  td{padding:3px 0}td:last-child{text-align:right}
  .muted{color:var(--ink-soft);font-size:12px}
  .num{color:var(--brand)}
</style>
</head>
<body>
<header>
  <span class="dot"></span>
  <h1>同绘 CoDraw · 运维面板</h1>
  <span class="badge" id="version">{{.Version}}</span>
  <span class="badge" id="instance">{{.InstanceID}}</span>
  <span class="muted" id="updated">每 2 秒刷新</span>
</header>
<main>
  <section>
    <h2>运行状态</h2>
    <dl>
      <dt>运行时长</dt><dd id="uptime">{{.UptimeSec}}s</dd>
      <dt>房间数</dt><dd id="rooms" class="num">{{.Rooms}}</dd>
      <dt>连接数</dt><dd id="conns" class="num">{{.Connections}}</dd>
      <dt>广播 p95</dt><dd id="p95">{{.BroadcastLatencyP95Ms}}ms</dd>
      <dt>消息速率 (入)</dt><dd id="inrate">{{.MsgPerSecIn}}/s</dd>
      <dt>消息速率 (出)</dt><dd id="outrate">{{.MsgPerSecOut}}/s</dd>
      <dt>慢消费者踢除</dt><dd id="kicks">{{.SlowConsumerKicks}}</dd>
      <dt>goroutines</dt><dd id="goroutines">{{.Goroutines}}</dd>
      <dt>堆内存</dt><dd id="heap">{{.HeapMB}}MB</dd>
      <dt>存储</dt><dd id="store">{{.Store}}</dd>
      <dt>广播后端</dt><dd id="broker">{{.Broker}}</dd>
    </dl>
  </section>
  <section>
    <h2>接收消息</h2>
    {{if .MessagesIn}}<table>{{range .MessagesIn}}<tr><td>{{.Key}}</td><td>{{.Value}}</td></tr>{{end}}</table>
    {{else}}<p class="muted">暂无数据</p>{{end}}
  </section>
  <section>
    <h2>发送消息</h2>
    {{if .MessagesOut}}<table>{{range .MessagesOut}}<tr><td>{{.Key}}</td><td>{{.Value}}</td></tr>{{end}}</table>
    {{else}}<p class="muted">暂无数据</p>{{end}}
  </section>
  <section>
    <h2>拒绝的操作</h2>
    {{if .Rejected}}<table>{{range .Rejected}}<tr><td>{{.Key}}</td><td>{{.Value}}</td></tr>{{end}}</table>
    {{else}}<p class="muted">暂无数据</p>{{end}}
  </section>
</main>
<script>
(function () {
  function set(id, v) { var el = document.getElementById(id); if (el) el.textContent = v; }
  // The token is read from the browser's own address bar rather than rendered into
  // the page, so the server never echoes it into HTML, logs or caches.
  var token = new URLSearchParams(location.search).get('token');
  var dataURL = '/ops/data' + (token ? '?token=' + encodeURIComponent(token) : '');
  function poll() {
    fetch(dataURL, {cache: 'no-store'})
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (d) {
        set('uptime', d.uptimeSec + 's');
        set('rooms', d.rooms);
        set('conns', d.connections);
        set('p95', d.broadcastLatencyP95Ms + 'ms');
        set('inrate', d.msgPerSecIn + '/s');
        set('outrate', d.msgPerSecOut + '/s');
        set('kicks', d.slowConsumerKicks);
        set('goroutines', d.goroutines);
        set('heap', d.heapMB.toFixed(1) + 'MB');
        set('store', d.store);
        set('broker', d.broker);
        set('updated', '更新于 ' + new Date().toLocaleTimeString());
      })
      .catch(function (e) { set('updated', '连接中断，重试中…'); });
  }
  setInterval(poll, 2000);
})();
</script>
<!-- request id: {{.RequestID}} -->
</body>
</html>
`))
