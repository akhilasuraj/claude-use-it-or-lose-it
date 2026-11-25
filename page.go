package main

import (
	"bytes"
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The page is self-contained (no external assets): it is served through the plugin resource
// route and shown inside the management panel's iframe.

const pageCSS = `
:root{
  color-scheme:light dark;
  --bg:#faf9f5;--bg-2:#f0eee8;
  --ink:#2d2a26;--muted:#6d6760;
  --glass:rgb(255 255 255 / .58);--glass-strong:rgb(255 255 255 / .85);
  --line:rgb(45 42 38 / .11);--shadow:rgb(45 42 38 / .13);
  --lavender:#c9b8ff;--mint:#a6ead0;--peach:#ffcfae;--pink:#ffb6ca;--sky:#b7dcff;--butter:#fff0a6;
  --liquid-ok:#7fd9b5;--liquid-warm:#ffc46b;--liquid-hot:#ff8fab;
  --btn-ink:#2d2a26;--focus:#2d2a26;
  --blob-opacity:.34;
  ` + chartTokensLight + `
}
/*DARK*/
*{box-sizing:border-box}
html{scroll-padding-top:96px;scrollbar-color:var(--lavender) transparent;background:var(--bg-2)}
body{margin:0;min-height:100dvh;color:var(--ink);background:linear-gradient(160deg,var(--bg),var(--bg-2));
  font:15px/1.5 ui-rounded,"SF Pro Rounded","Nunito","Segoe UI Variable Text","Segoe UI",system-ui,sans-serif}
:focus-visible{outline:3px solid var(--focus);outline-offset:3px;border-radius:12px}
.sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip-path:inset(50%);white-space:nowrap}

/* ---- floating bubbles ---- */
.blobs{position:fixed;inset:0;z-index:-1;overflow:clip;pointer-events:none}
.blobs i{position:absolute;border-radius:50%;filter:blur(36px);opacity:var(--blob-opacity)}
.blobs i:nth-child(1){width:340px;height:340px;background:var(--lavender);top:-80px;left:-60px;--dx:60px;--dy:40px;--t:26s}
.blobs i:nth-child(2){width:260px;height:260px;background:var(--mint);top:30%;right:-70px;--dx:-50px;--dy:70px;--t:31s}
.blobs i:nth-child(3){width:220px;height:220px;background:var(--peach);bottom:-60px;left:18%;--dx:70px;--dy:-50px;--t:23s}
.blobs i:nth-child(4){width:180px;height:180px;background:var(--pink);top:12%;left:46%;--dx:-40px;--dy:60px;--t:35s}
.blobs i:nth-child(5){width:140px;height:140px;background:var(--sky);bottom:14%;right:22%;--dx:40px;--dy:-40px;--t:28s}
@media (prefers-reduced-motion:no-preference){
  .blobs i{animation:drift var(--t) ease-in-out infinite alternate}
}
@keyframes drift{to{translate:var(--dx) var(--dy);scale:1.12}}
html[data-motion="paused"] *,html[data-motion="paused"] *::before,html[data-motion="paused"] *::after{animation-play-state:paused !important}

/* ---- sticky glass header ---- */
main{max-width:1080px;margin:0 auto;padding:0 16px 40px}
.bar{position:sticky;top:0;z-index:5;container-type:scroll-state;container-name:bar;padding-top:12px;margin:0 -4px}
.bar-inner{display:flex;flex-wrap:wrap;gap:12px;align-items:center;justify-content:space-between;
  padding:12px 14px 12px 16px;border-radius:28px;background:var(--glass);border:1px solid var(--line);
  -webkit-backdrop-filter:blur(18px) saturate(1.4);backdrop-filter:blur(18px) saturate(1.4);
  transition:box-shadow .3s,background-color .3s}
@container bar scroll-state(stuck:top){
  .bar-inner{background:var(--glass-strong);box-shadow:0 12px 32px -12px var(--shadow)}
}
.brand{display:flex;align-items:center;gap:12px;min-width:0}
.logo{flex:none;width:40px;height:40px;border-radius:50%;
  background:conic-gradient(from 200deg,var(--lavender),var(--pink),var(--peach),var(--mint),var(--sky),var(--lavender));
  box-shadow:inset 0 -6px 12px rgb(0 0 0 / .08),0 6px 16px -6px var(--shadow)}
h1{font-size:18px;line-height:1.2;margin:0;letter-spacing:-.01em}
.sub{color:var(--muted);font-size:13px}
.actions{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.btn{--spring:linear(0,.016 .5%,.06 1%,.226 2%,1.116 5.4%,1.375 6.6%,1.527 7.7%,1.565 8.2%,1.585 8.8%,1.581 9.3%,1.559 9.8%,1.458 10.9%,.937 14.3%,.784 15.5%,.693 16.6%,.67 17.1%,.657 17.7%,.671 18.7%,.729 19.8%,1.042 23.3%,1.13 24.5%,1.182 25.6%,1.201 26.7%,1.192 27.7%,1.156 28.8%,.977 32.2%,.925 33.4%,.894 34.5%,.882 35.6%,.887 36.6%,.907 37.7%,1.045 42.4%,1.069 44.5%,1.059 46.3%,.979 50.9%,.96 53.4%,.966 55.3%,1.013 59.9%,1.024 62.3%,.986 71.2%,1.008 79.9%,.995 88.9%,1);
  appearance:none;border:0;cursor:pointer;font:inherit;font-weight:700;font-size:14px;text-decoration:none;
  display:inline-flex;align-items:center;gap:8px;min-height:40px;padding:8px 16px;border-radius:999px;color:var(--btn-ink);
  background:linear-gradient(135deg,var(--lavender),var(--pink));box-shadow:inset 0 2px 0 rgb(255 255 255 / .45),0 8px 18px -8px var(--shadow);
  transition:scale .3s ease-out;transition-timing-function:var(--spring)}
.btn.ghost{background:var(--glass-strong);color:var(--ink);box-shadow:inset 0 0 0 1px var(--line)}
@media (prefers-reduced-motion:no-preference){
  .btn:hover{scale:1.06}
  .btn:active{scale:.96}
  .btn{transition-duration:.8s}
}
.btn .dot{width:10px;height:10px;border-radius:50%;background:currentColor;opacity:.7}
html[data-busy] .btn.refresh .dot{animation:pulse 1s ease-in-out infinite alternate}
@keyframes pulse{to{scale:1.6;opacity:.25}}

/* ---- notices ---- */
.notice{margin:14px 0 0;padding:10px 14px;border-radius:18px;background:var(--glass);border:1px solid var(--line)}
.notice.bad{background:color-mix(in srgb,var(--pink) 35%,var(--glass))}
.notice.shadow{background:color-mix(in srgb,var(--lavender) 35%,var(--glass))}
.about code{font-size:12px;overflow-wrap:anywhere}

/* ---- hero bubble ---- */
.hero{display:grid;grid-template-columns:auto 1fr;gap:20px;align-items:center;margin:18px 0;padding:20px;border-radius:32px;
  background:var(--glass);border:1px solid var(--line);box-shadow:0 18px 40px -24px var(--shadow)}
.hero .bubble{--size:120px}
.eyebrow{display:inline-block;font-size:12px;font-weight:800;letter-spacing:.08em;text-transform:uppercase;
  padding:3px 10px;border-radius:999px;background:var(--mint);color:#1f3b30}
.hero h2{margin:8px 0 2px;font-size:clamp(18px,4.5cqi,24px);overflow-wrap:anywhere}
.hero p{margin:0;color:var(--muted)}
.chips{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.chip{font-size:12.5px;font-weight:600;padding:3px 10px;border-radius:999px;background:var(--glass-strong);border:1px solid var(--line)}
.hero-wrap{container-type:inline-size}
@container (width < 460px){.hero{grid-template-columns:1fr;justify-items:center;text-align:center}.chips{justify-content:center}}

/* ---- liquid bubble gauge ---- */
.bubble{--size:48px;--fill:0%;--liquid:var(--liquid-ok);position:relative;flex:none;width:var(--size);height:var(--size);border-radius:50%;
  overflow:clip;background:radial-gradient(circle at 50% 120%,color-mix(in srgb,var(--liquid) 18%,transparent),transparent 70%),var(--glass-strong);
  box-shadow:inset 0 0 0 2px color-mix(in srgb,var(--liquid) 55%,transparent),0 8px 20px -10px var(--shadow)}
.bubble::before,.bubble::after{content:"";position:absolute;left:50%;width:220%;aspect-ratio:1;
  top:calc(100% - var(--fill));translate:-50% 0;border-radius:42%}
.bubble::before{background:var(--liquid);opacity:.95}
.bubble::after{background:color-mix(in srgb,var(--liquid) 55%,white);opacity:.55;border-radius:38%;top:calc(100% - var(--fill) - 3%)}
@media (prefers-reduced-motion:no-preference){
  .bubble::before{animation:wave 7s linear infinite}
  .bubble::after{animation:wave 11s linear infinite reverse}
}
@keyframes wave{to{rotate:360deg}}
.tone-warm{--liquid:var(--liquid-warm)}
.tone-hot{--liquid:var(--liquid-hot)}

/* ---- table as bubble rows ---- */
.table-wrap{container-type:inline-size;container-name:tbl;overflow-x:auto;border-radius:28px}
table{width:100%;border-collapse:separate;border-spacing:0 10px}
caption{text-align:left;font-weight:800;font-size:15px;padding:4px 6px 0}
thead th{font-size:11.5px;font-weight:800;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);text-align:left;padding:6px 14px 0}
tbody th,tbody td{background:var(--glass);padding:14px;text-align:left;vertical-align:middle;
  border-block:1px solid var(--line);-webkit-backdrop-filter:blur(10px);backdrop-filter:blur(10px)}
tbody th{border-inline-start:1px solid var(--line);border-radius:24px 0 0 24px;font-weight:600}
tbody td:last-child{border-inline-end:1px solid var(--line);border-radius:0 24px 24px 0}
tbody tr.is-next th,tbody tr.is-next td{background:color-mix(in srgb,var(--mint) 30%,var(--glass))}
.acct{display:flex;align-items:center;gap:12px;min-width:0}
.rank{flex:none;display:grid;place-items:center;width:34px;height:34px;border-radius:50%;font-weight:800;font-variant-numeric:tabular-nums;
  background:var(--lavender);color:#2b2540;box-shadow:inset 0 2px 0 rgb(255 255 255 / .5)}
tr.is-next .rank{background:var(--mint);color:#1f3b30}
.name{display:block;overflow-wrap:break-word}
.meta{display:block;color:var(--muted);font-size:12.5px;font-weight:400}
.gauge{display:flex;align-items:center;gap:10px}
.pct{font-weight:700;font-variant-numeric:tabular-nums}
.num{font-variant-numeric:tabular-nums;font-weight:700}

/* native <progress>, fully custom styled */
progress{--progress-bar-radius:1e5px;--progress-bar-fill:var(--liquid);--liquid:var(--liquid-ok);
  appearance:none;border:0 solid;overflow:clip;display:block;inline-size:120px;block-size:10px;margin-top:6px;
  border-radius:var(--progress-bar-radius);background:color-mix(in srgb,var(--lavender) 22%,transparent);box-shadow:inset 0 1px 2px rgb(0 0 0 / .1)}
progress::-webkit-progress-bar{background:transparent}
progress::-webkit-progress-value{background:var(--progress-bar-fill);border-radius:var(--progress-bar-radius);transition:inline-size .3s}
progress::-moz-progress-bar{background:var(--progress-bar-fill);border-radius:var(--progress-bar-radius)}

.pill{display:inline-block;font-size:12.5px;font-weight:700;padding:3px 10px;border-radius:999px}
.pill.ok{background:var(--mint);color:#1f3b30}
.pill.warn{background:var(--butter);color:#4a3d00}
.pill.bad{background:var(--pink);color:#4a1424}
.pill.next{background:var(--lavender);color:#2d2a26}
.pill.idle{background:var(--glass-strong);color:var(--ink);box-shadow:inset 0 0 0 1px var(--line)}

/* first paint only: rows pop in with a spring */
@media (prefers-reduced-motion:no-preference){
  html:not([data-live]) tbody tr{animation:pop .7s backwards;animation-delay:calc(var(--i) * 70ms);
    animation-timing-function:linear(0,.25 7%,.78 17%,1.06 27%,1.1 33%,1.03 45%,.99 60%,1)}
}
@keyframes pop{from{opacity:0;translate:0 14px;scale:.97}}

/* narrow: stack each row into a bubble card */
@container tbl (width < 700px){
  table,thead,tbody,tr,th,td,caption{display:block}
  thead{display:none}
  caption{padding-bottom:10px}
  table{border-spacing:0}
  tbody tr{margin-block-end:14px;border-radius:28px;background:var(--glass);border:1px solid var(--line);overflow:clip}
  tbody th,tbody td{background:transparent;border:0;border-radius:0;padding:10px 16px;-webkit-backdrop-filter:none;backdrop-filter:none}
  tbody th{padding-top:16px}
  tbody td:last-child{border:0;border-radius:0;padding-bottom:16px}
  tbody td{display:grid;grid-template-columns:8.5rem 1fr;gap:10px;align-items:center}
  tbody td::before{content:attr(data-label);font-size:11.5px;font-weight:800;letter-spacing:.06em;text-transform:uppercase;color:var(--muted)}
  tbody tr.is-next{background:color-mix(in srgb,var(--mint) 30%,var(--glass))}
  tbody tr.is-next th,tbody tr.is-next td{background:transparent}
}

/* ---- info popover ---- */
.btn.icon{width:40px;padding:0;justify-content:center}
.btn.icon svg{width:20px;height:20px}
#info{anchor-name:--info}
.about{width:min(380px,calc(100vw - 32px));padding:16px 18px;border-radius:24px;border:1px solid var(--line);
  background:var(--glass-strong);-webkit-backdrop-filter:blur(18px) saturate(1.4);backdrop-filter:blur(18px) saturate(1.4);
  color:var(--muted);font-size:13px;box-shadow:0 18px 40px -16px var(--shadow)}
.about h2{margin:0 0 6px;font-size:14px;color:var(--ink)}
.about p{margin:6px 0 0}
.about b{color:var(--ink)}
/* Tether below the button where anchor positioning exists; otherwise keep the default centred popover. */
@supports (position-area:block-end){
  .about{position-anchor:--info;inset:auto;margin:8px 8px 0;position-area:block-end span-inline-start;
    position-try-fallbacks:flip-block,flip-inline,flip-block flip-inline}
}
/* motion switch */
.setting{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-top:12px;padding-top:12px;border-top:1px solid var(--line);color:var(--ink);font-weight:600;cursor:pointer}
.setting .meta{font-weight:400}
.setting input{appearance:none;flex:none;position:relative;width:44px;height:26px;margin:0;border-radius:999px;cursor:pointer;
  background:color-mix(in srgb,var(--muted) 30%,transparent);box-shadow:inset 0 0 0 1px var(--line);transition:background-color .2s}
.setting input::before{content:"";position:absolute;top:3px;left:3px;width:20px;height:20px;border-radius:50%;background:#fff;
  box-shadow:0 1px 3px rgb(0 0 0 / .25);transition:translate .2s ease-out}
.setting input:checked{background:var(--mint)}
.setting input:checked::before{translate:18px 0}
@media (prefers-reduced-motion:no-preference){
  .about{transition:opacity .2s,translate .25s ease-out,display .25s allow-discrete,overlay .25s allow-discrete}
  .about:not(:popover-open){opacity:0;translate:0 -6px}
  @starting-style{.about:popover-open{opacity:0;translate:0 -6px}}
}
`

// darkTokens match the panel's dark theme; they also apply when the OS is dark and no panel theme is set.
const darkTokens = `
    --bg:#1d1b18;--bg-2:#151412;
    --ink:#f6f4f1;--muted:#c9c3bb;
    --glass:rgb(42 39 35 / .66);--glass-strong:rgb(46 42 38 / .9);
    --line:rgb(246 244 241 / .1);--shadow:rgb(0 0 0 / .42);
    --lavender:#a898e0;--mint:#6fc7a4;--peach:#e3a27f;--pink:#e08ea4;--sky:#86afd8;--butter:#d6c46a;
    --liquid-ok:#6fcfa8;--liquid-warm:#eeb257;--liquid-hot:#ee839d;
    --btn-ink:#151412;--focus:#f6f4f1;
    --blob-opacity:.14;
` + chartTokensDark

// whiteTokens match the panel's "white" theme: pure white surfaces, same pastel accents.
const whiteTokens = `
    --bg:#ffffff;--bg-2:#f6f6f6;
    --glass:rgb(255 255 255 / .7);--glass-strong:rgb(255 255 255 / .9);
    --line:rgb(0 0 0 / .1);--shadow:rgb(0 0 0 / .1);
    --blob-opacity:.24;--surface:#ffffff;
`

// pageStyles is pageCSS with the dark palette wired to both the OS setting and data-theme,
// which the inline head script copies from the management panel's own theme switcher.
var pageStyles = strings.Replace(pageCSS, "/*DARK*/",
	"@media (prefers-color-scheme:dark){:root:not([data-theme]){"+darkTokens+"}}\n"+
		":root[data-theme=\"dark\"]{color-scheme:dark;"+darkTokens+"}\n"+
		":root[data-theme=\"light\"]{color-scheme:light}\n"+
		":root[data-theme=\"white\"]{color-scheme:light;"+whiteTokens+"}\n", 1) + chartCSS

// themeJS runs in <head> before first paint so the page never flashes the wrong theme.
// The page is served from the panel's origin, so it can read the panel's <html data-theme>
// ("dark", "white", or absent for the default cream light theme; "auto" is already resolved by the panel) and follow
// later switches. Opened on its own (or if access is blocked) it falls back to the OS setting.
const themeJS = `
(()=>{const root=document.documentElement;let p;
try{if(window.parent!==window)p=window.parent.document.documentElement;}catch{}
if(!p)return;
const sync=()=>{const t=p.getAttribute('data-theme');root.dataset.theme=t==='dark'||t==='white'?t:'light';};
sync();new MutationObserver(sync).observe(p,{attributes:true,attributeFilter:['data-theme']});})();
`

// infoIcon is an inline "i in a circle" glyph for the header's info button.
const infoIcon = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="9.5"/><path d="M12 11v6"/><circle cx="12" cy="7.6" r=".6" fill="currentColor"/></svg>`

// pageJS updates the page in place instead of reloading: live countdowns, periodic refresh of
// the content, an in-place "Refresh now", and the motion switch in the info popover.
const pageJS = `
(()=>{
const root=document.documentElement;
const live=document.getElementById('live');
const fmt=ms=>{ms=Math.abs(ms);const s=Math.floor(ms/1e3),m=Math.floor(s/60),h=Math.floor(m/60),d=Math.floor(h/24);
  if(s<60)return s+'s';if(m<60)return m+'m';if(h<48)return h+'h '+(m%60)+'m';return d+'d '+(h%24)+'h';};
function tick(){const now=Date.now();
  document.querySelectorAll('time[data-in]').forEach(t=>{const ms=Date.parse(t.dateTime)-now;t.textContent=ms>0?'in '+fmt(ms):'now';});
  document.querySelectorAll('time[data-ago]').forEach(t=>{t.textContent=fmt(now-Date.parse(t.dateTime))+' ago';});}
let timer;
function schedule(){clearTimeout(timer);const s=Number(root.dataset.reload)||60;timer=setTimeout(()=>pull('status'),s*1e3);}
async function pull(url){
  root.dataset.busy='';
  try{
    const r=await fetch(url,{cache:'no-store'});if(!r.ok)throw new Error('HTTP '+r.status);
    const doc=new DOMParser().parseFromString(await r.text(),'text/html');
    const next=doc.getElementById('app');const focusId=document.activeElement&&document.activeElement.id;
    if(next){document.getElementById('app').replaceWith(next);if(window.bindCharts)window.bindCharts();}
    root.dataset.reload=doc.documentElement.dataset.reload||'60';
    const n=next&&next.querySelector('.notice');if(n&&live)live.textContent=n.textContent;
    if(focusId){const el=document.getElementById(focusId);if(el)el.focus();}
  }catch(e){if(live)live.textContent='Could not update: '+e.message;}
  finally{delete root.dataset.busy;root.dataset.live='';tick();schedule();}
}
document.addEventListener('click',e=>{
  const a=e.target.closest('a.refresh');if(a){e.preventDefault();pull('status?action=refresh');return;}
});
// Motion switch lives in the info popover (outside #app), so it survives in-place refreshes.
const sw=document.getElementById('motion');
try{if(localStorage.getItem('rs-motion')==='paused')root.dataset.motion='paused';}catch{}
if(sw){sw.checked=root.dataset.motion!=='paused';
  sw.addEventListener('change',()=>{if(sw.checked)delete root.dataset.motion;else root.dataset.motion='paused';
    try{localStorage.setItem('rs-motion',sw.checked?'on':'paused');}catch{}});}
tick();setInterval(tick,15e3);schedule();
})();
`

func toneClass(pct float64) string {
	switch {
	case pct >= 90:
		return "tone-hot"
	case pct >= 70:
		return "tone-warm"
	default:
		return ""
	}
}

func bubbleHTML(pct float64, size int) string {
	clamped := math.Max(0, math.Min(100, pct))
	sizeStyle := ""
	if size > 0 {
		sizeStyle = fmt.Sprintf("--size:%dpx;", size)
	}
	return fmt.Sprintf(`<span class="bubble %s" style="%s--fill:%.0f%%" aria-hidden="true"></span>`,
		toneClass(pct), sizeStyle, clamped)
}

// breakableLabel escapes an account label and lets long e-mail addresses wrap at the "@"
// rather than mid-word.
func breakableLabel(label string) string {
	return strings.Replace(html.EscapeString(label), "@", "<wbr>@", 1)
}

func timeTag(t time.Time, attr, text string) string {
	return fmt.Sprintf(`<time datetime="%s" %s>%s</time>`, t.UTC().Format(time.RFC3339), attr, html.EscapeString(text))
}

func renderPage(notice string, now time.Time) []byte {
	cfg := loadedConfig()
	accounts := snapshotAccounts()

	state.mu.RLock()
	lastRun, nextRun, running, lastErr := state.lastRun, state.nextRun, state.running, state.lastErr
	state.mu.RUnlock()

	stats.mu.Lock()
	counts := make(map[string]int64, len(stats.counts))
	for k, v := range stats.counts {
		counts[k] = v
	}
	served := make(map[string]int64, len(stats.served))
	for k, v := range stats.served {
		served[k] = v
	}
	lastPickID, lastPickAt := stats.lastID, stats.lastAt
	stats.mu.Unlock()

	// Rank enabled accounts exactly the way the scheduler does.
	var cands []pluginapi.SchedulerAuthCandidate
	for id, acc := range accounts {
		if !acc.Disabled {
			cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: claudeProvider, Priority: acc.Priority})
		}
	}
	ranked := rankCandidates(cands, accounts, cfg, now)
	window := cfg.window()
	paces := map[string]pace{}
	cyclePoints := map[string][]point{}
	for _, rc := range ranked {
		acc := accounts[rc.id]
		if w, ok := acc.Windows[window]; ok && w.ResetsAt.After(now) {
			pts := history.cycle(historyKey(acc), window, w.ResetsAt)
			cyclePoints[rc.id] = pts
			paces[rc.id] = paceFor(pts, w, now)
		}
	}

	reload := 60
	if running || notice != "" {
		reload = 4
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<!doctype html><html lang="en" data-reload="%d"><head><meta charset="utf-8">`, reload)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark">`)
	fmt.Fprintf(&b, `<noscript><meta http-equiv="refresh" content="%d;url=status"></noscript>`, reload)
	b.WriteString(`<title>` + pluginTitle + `</title><script>` + themeJS + `</script><style>` + pageStyles + `</style></head><body>`)
	b.WriteString(`<div class="blobs" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></div>`)
	b.WriteString(`<div id="live" class="sr-only" role="status" aria-live="polite"></div>`)
	b.WriteString(`<main id="app">`)

	// Sticky header.
	b.WriteString(`<header class="bar"><div class="bar-inner"><div class="brand"><span class="logo" aria-hidden="true"></span><div><h1>` + pluginTitle + `</h1><div class="sub">`)
	switch {
	case running:
		b.WriteString(`Checking usage…`)
	case lastRun.IsZero():
		b.WriteString(`First check pending`)
	default:
		fmt.Fprintf(&b, `Updated %s`, timeTag(lastRun, "data-ago", humanDuration(now.Sub(lastRun))+" ago"))
		if !nextRun.IsZero() && nextRun.After(now) {
			fmt.Fprintf(&b, ` · next %s`, timeTag(nextRun, "data-in", "in "+humanDuration(nextRun.Sub(now))))
		}
	}
	fmt.Fprintf(&b, ` · every %s, plus live updates from responses</div></div></div>`, humanDuration(cfg.interval()))
	b.WriteString(`<div class="actions"><button type="button" id="info" class="btn ghost icon" popovertarget="about" aria-label="How it works">` + infoIcon + `</button>`)
	b.WriteString(`<a id="refresh" class="btn refresh" href="status?action=refresh"><span class="dot" aria-hidden="true"></span>Refresh now</a></div>`)
	b.WriteString(`</div></header>`)

	if notice != "" {
		fmt.Fprintf(&b, `<p class="notice">%s</p>`, html.EscapeString(notice))
	}
	if lastErr != "" {
		fmt.Fprintf(&b, `<p class="notice bad">Last check failed: %s</p>`, html.EscapeString(lastErr))
	}
	for _, rc := range ranked {
		if acc := accounts[rc.id]; acc.LoginExpired {
			fmt.Fprintf(&b, `<section class="notice bad login" aria-label="Login expired"><b>Login expired: %s.</b> Log in to this account again on the Auth Files page. Until then this plugin cannot read its usage.</section>`,
				html.EscapeString(firstNonEmpty(acc.Label, acc.FileName, acc.ID)))
		}
	}
	if cfg.Shadow {
		b.WriteString(`<p class="notice shadow"><b>Watch only.</b> Requests use the normal routing. The page shows which account this plugin would pick, so you can compare before turning it on.</p>`)
	}

	// Hero: the account in use, i.e. the one the scheduler picks for every request right now.
	b.WriteString(`<div class="hero-wrap"><section class="hero" aria-labelledby="next-title">`)
	if len(ranked) > 0 && ranked[0].hasReset {
		top := ranked[0]
		acc := accounts[top.id]
		weekly := acc.Windows[window]
		b.WriteString(bubbleHTML(weekly.Utilization, 120))
		eyebrow := "In use"
		if cfg.Shadow {
			eyebrow = "Would pick"
		}
		fmt.Fprintf(&b, `<div><span class="eyebrow">%s</span>`, eyebrow)
		fmt.Fprintf(&b, `<h2 id="next-title">%s</h2>`, html.EscapeString(firstNonEmpty(acc.Label, acc.FileName, acc.ID)))
		fmt.Fprintf(&b, `<p>Weekly limit resets %s · %s</p>`,
			timeTag(weekly.ResetsAt, "data-in", "in "+humanDuration(weekly.ResetsAt.Sub(now))),
			html.EscapeString(weekly.ResetsAt.Local().Format("Mon 2 Jan, 15:04")))
		b.WriteString(`<div class="chips">`)
		fmt.Fprintf(&b, `<span class="chip">Weekly %.0f%% used</span>`, weekly.Utilization)
		if fh, ok := acc.Windows["five_hour"]; ok {
			fmt.Fprintf(&b, `<span class="chip">5-hour %.0f%% used</span>`, fh.Utilization)
		}
		if headline, _ := paceSummary(paces[top.id], now); headline != "" {
			fmt.Fprintf(&b, `<span class="chip">%s</span>`, html.EscapeString(headline))
		}
		b.WriteString(`</div></div>`)
	} else {
		b.WriteString(bubbleHTML(0, 120))
		b.WriteString(`<div><span class="eyebrow">Waiting</span><h2 id="next-title">No reset data yet</h2>`)
		b.WriteString(`<p>Until reset times arrive, requests use the normal routing strategy.</p></div>`)
	}
	b.WriteString(`</section></div>`)

	// Order table.
	b.WriteString(`<div class="table-wrap" tabindex="0" role="region" aria-labelledby="order-caption"><table>`)
	fmt.Fprintf(&b, `<caption id="order-caption">Pick order · soonest %s weekly reset first</caption>`, html.EscapeString(windowLabel(window)))
	picksHead := "Picks"
	if cfg.Shadow {
		picksHead = "Would pick"
	}
	fmt.Fprintf(&b, `<thead><tr><th scope="col">Account</th><th scope="col">Weekly reset</th><th scope="col">Weekly used</th><th scope="col">At this pace</th><th scope="col">5-hour used</th><th scope="col">%s</th><th scope="col">Status</th></tr></thead><tbody>`, picksHead)
	if len(ranked) == 0 {
		b.WriteString(`<tr style="--i:0"><th scope="row" colspan="7">No enabled Claude accounts found yet.</th></tr>`)
	}
	for i, rc := range ranked {
		acc := accounts[rc.id]
		label := firstNonEmpty(acc.Label, acc.FileName, acc.ID)
		cls := ""
		if i == 0 && rc.hasReset && !rc.exhausted {
			cls = ` class="is-next"`
		}
		fmt.Fprintf(&b, `<tr%s style="--i:%d">`, cls, i)
		fmt.Fprintf(&b, `<th scope="row"><span class="acct"><span class="rank"><span class="sr-only">Rank </span>%d</span><span><span class="name">%s</span><span class="meta">priority %d</span></span></span></th>`,
			i+1, breakableLabel(label), acc.Priority)

		weekly, hasWeekly := acc.Windows[window]
		if hasWeekly && !weekly.ResetsAt.IsZero() {
			fmt.Fprintf(&b, `<td data-label="Weekly reset"><span><span class="pct">%s</span><span class="meta">%s</span></span></td>`,
				timeTag(weekly.ResetsAt, "data-in", "in "+humanDuration(weekly.ResetsAt.Sub(now))),
				html.EscapeString(weekly.ResetsAt.Local().Format("Mon 2 Jan, 15:04")))
		} else {
			b.WriteString(`<td data-label="Weekly reset"><span class="meta">unknown</span></td>`)
		}

		if hasWeekly {
			fmt.Fprintf(&b, `<td data-label="Weekly used"><span class="gauge">%s<span class="pct">%.0f%%</span></span></td>`, bubbleHTML(weekly.Utilization, 0), weekly.Utilization)
		} else {
			b.WriteString(`<td data-label="Weekly used"><span class="meta">–</span></td>`)
		}

		if p, ok := paces[rc.id]; ok {
			headline, detail := paceSummary(p, now)
			if headline == "" {
				fmt.Fprintf(&b, `<td data-label="At this pace"><span class="meta">%s</span></td>`, html.EscapeString(detail))
			} else {
				fmt.Fprintf(&b, `<td data-label="At this pace"><span><span class="pct">%s</span><span class="meta">%s</span></span></td>`, html.EscapeString(headline), html.EscapeString(detail))
			}
		} else {
			b.WriteString(`<td data-label="At this pace"><span class="meta">–</span></td>`)
		}

		if fh, ok := acc.Windows["five_hour"]; ok {
			reset := ""
			if fh.ResetsAt.After(now) {
				reset = `<span class="meta">resets ` + timeTag(fh.ResetsAt, "data-in", "in "+humanDuration(fh.ResetsAt.Sub(now))) + `</span>`
			}
			fmt.Fprintf(&b, `<td data-label="5-hour used"><span><span class="pct">%.0f%%</span><progress class="%s" max="100" value="%.0f" aria-label="5-hour usage, %s"></progress>%s</span></td>`,
				fh.Utilization, toneClass(fh.Utilization), math.Max(0, math.Min(100, fh.Utilization)), html.EscapeString(label), reset)
		} else {
			b.WriteString(`<td data-label="5-hour used"><span class="meta">–</span></td>`)
		}

		pickNote := ""
		if rc.id == lastPickID && !lastPickAt.IsZero() {
			pickNote = `<span class="meta">last ` + timeTag(lastPickAt, "data-ago", humanDuration(now.Sub(lastPickAt))+" ago") + `</span>`
		}
		fmt.Fprintf(&b, `<td data-label="%s"><span><span class="num">%d</span><span class="meta">%d sent</span>%s</span></td>`, picksHead, counts[rc.id], served[rc.id], pickNote)

		b.WriteString(`<td data-label="Status"><span>`)
		switch {
		case acc.LoginExpired:
			b.WriteString(`<span class="pill bad">Login expired</span><span class="meta">log in again on the Auth Files page</span>`)
		case acc.Error != "":
			fmt.Fprintf(&b, `<span class="pill bad">Check failed</span><span class="meta">%s</span>`, html.EscapeString(acc.Error))
		case rc.exhausted:
			b.WriteString(`<span class="pill warn">Limit reached</span><span class="meta">moved to the bottom</span>`)
		case !rc.hasReset:
			b.WriteString(`<span class="pill idle">No reset data</span><span class="meta">ordered by priority</span>`)
		case i == 0 && cfg.Shadow:
			b.WriteString(`<span class="pill ok">Would pick</span>`)
		case i == 0:
			b.WriteString(`<span class="pill ok">In use</span>`)
		case i == 1:
			b.WriteString(`<span class="pill next">Next</span>`)
		default:
			b.WriteString(`<span class="pill idle">Standby</span>`)
		}
		if acc.Status != "" && acc.Status != "active" {
			fmt.Fprintf(&b, `<span class="meta">host: %s</span>`, html.EscapeString(acc.Status))
		}
		// The header already shows when data was refreshed; only call out rows still showing
		// older data because their latest fetch failed.
		if !acc.FetchedAt.IsZero() && lastRun.Sub(acc.FetchedAt) > time.Minute {
			fmt.Fprintf(&b, `<span class="meta">data from %s</span>`, timeTag(acc.FetchedAt, "data-ago", humanDuration(now.Sub(acc.FetchedAt))+" ago"))
		}
		b.WriteString(`</span></td></tr>`)
	}
	b.WriteString(`</tbody></table></div>`)

	// Weekly usage charts, in pick order.
	var charts strings.Builder
	for _, rc := range ranked {
		acc := accounts[rc.id]
		w, ok := acc.Windows[window]
		if !ok || !w.ResetsAt.After(now) || acc.Error != "" {
			continue
		}
		label := firstNonEmpty(acc.Label, acc.FileName, acc.ID)
		headline, detail := paceSummary(paces[rc.id], now)
		if headline == "" {
			headline = fmt.Sprintf("%.0f%% used", w.Utilization)
		}
		fmt.Fprintf(&charts, `<figure class="chart-card"><figcaption><span class="name">%s</span><span class="meta">%s · resets %s</span><span class="meta">%s</span></figcaption>%s</figure>`,
			breakableLabel(label), html.EscapeString(headline), html.EscapeString(w.ResetsAt.Local().Format("Mon 15:04")), html.EscapeString(detail),
			renderChart(chartInput{Label: label, Window: w, Points: cyclePoints[rc.id], Pace: paces[rc.id], Now: now}))
	}
	if charts.Len() > 0 {
		b.WriteString(`<section class="weeks" aria-labelledby="weeks-title"><h2 id="weeks-title">This week</h2>`)
		b.WriteString(`<p>Solid line: measured usage. Dashed line: the same pace continued to the reset. The pink bracket is what would be left unused.</p>`)
		b.WriteString(`<div class="charts">` + charts.String() + `</div></section>`)
	}

	b.WriteString(`</main>`)
	// Secondary explanation, opened from the header's info button. It sits outside #app so the
	// periodic in-place refresh never closes it; anchor-name on #info re-tethers it after a swap.
	b.WriteString(`<div id="about" class="about" popover><h2>How it works</h2>`)
	b.WriteString(`<p><b>How it picks.</b> The proxy offers only accounts it considers usable; rate-limited, cooling-down and disabled accounts are already left out. Of those, the one whose weekly limit resets soonest wins.</p>`)
	b.WriteString(`<p><b>When it calls Anthropic.</b> Only on the timer, shortly after a stored reset passes, or when you press Refresh (at most once a minute). Picking an account never makes a network call. Between checks, the usage shown with every Claude response keeps the numbers current.</p>`)
	b.WriteString(`<p><b>Cooldowns.</b> If the proxy is still holding back an account that a check shows has room again, for example after an early reset, the plugin clears that cooldown.</p>`)
	b.WriteString(`<p><b>History.</b> Readings are kept for 15 days in <code>` + html.EscapeString(cfg.historyPath()) + `</code>. Picks and sent counts start at zero when the proxy starts.</p>`)
	b.WriteString(`<p><b>Settings.</b> Plugins page → ` + pluginName + `.</p>`)
	// switch is native in Safari; role="switch" plus our own styling covers other browsers.
	b.WriteString(`<label class="setting"><span>Background motion<span class="meta">Also off when your system reduces motion.</span></span><input id="motion" type="checkbox" switch role="switch" checked></label>`)
	b.WriteString(`</div>`)
	b.WriteString(`<script>` + chartJS + pageJS + `</script></body></html>`)
	return b.Bytes()
}

// windowLabel is the display name of a weekly usage window.
func windowLabel(key string) string {
	switch key {
	case "seven_day_opus":
		return windowOpus
	case "seven_day_sonnet":
		return windowSonnet
	default:
		return "all-models"
	}
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	}
}
