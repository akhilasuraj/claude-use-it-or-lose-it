package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"strings"
	"time"
)

// Weekly usage charts: one small chart per account (each has its own week, so they cannot
// share an x axis). The solid line is measured usage, the dashed line continues it at the
// current pace to the reset, and the bracket at the reset shows what would be left unused.

const (
	chartW, chartH = 340.0, 150.0
	chartL, chartR = 34.0, 64.0 // room for y ticks / the end label
	chartT, chartB = 14.0, 24.0 // room for "now" / day labels
	chartCSS       = `
.weeks{margin-top:22px}
.weeks h2{font-size:15px;margin:0 6px 4px}
.weeks>p{margin:0 6px 12px;color:var(--muted);font-size:13px}
.charts{display:grid;grid-template-columns:repeat(auto-fill,minmax(min(100%,300px),1fr));gap:14px}
.chart-card{position:relative;margin:0;padding:14px 14px 10px;border-radius:26px;background:var(--glass);border:1px solid var(--line)}
.chart-card figcaption{display:flex;flex-direction:column;gap:2px;margin:0 4px 6px}
.chart-card .name{font-weight:700;overflow-wrap:anywhere}
.chart-card svg{display:block;width:100%;height:auto;overflow:visible;touch-action:pan-y}
.chart-card svg:focus-visible{outline:3px solid var(--focus);outline-offset:4px;border-radius:12px}
.c-grid{stroke:var(--line);stroke-width:1}
.c-tick{fill:var(--muted);font-size:10px;font-variant-numeric:tabular-nums}
.c-area{fill:var(--series);opacity:.1}
.c-line{fill:none;stroke:var(--series);stroke-width:2;stroke-linejoin:round;stroke-linecap:round}
.c-proj{fill:none;stroke:var(--series);stroke-width:2;stroke-dasharray:4 4;stroke-linecap:round}
.c-dot{fill:var(--series);stroke:var(--surface);stroke-width:2}
.c-now{stroke:var(--muted);stroke-width:1}
.c-unused{fill:var(--unused);opacity:.16}
.c-unused-edge{stroke:var(--unused);stroke-width:2;stroke-linecap:round}
.c-label{fill:var(--ink);font-size:11px;font-weight:700}
.c-sub{fill:var(--muted);font-size:10px}
.c-hair{stroke:var(--ink);stroke-width:1;opacity:.5;pointer-events:none}
.c-tip{position:absolute;z-index:2;pointer-events:none;padding:6px 10px;border-radius:12px;font-size:12px;white-space:nowrap;
  background:var(--glass-strong);border:1px solid var(--line);box-shadow:0 8px 20px -10px var(--shadow);translate:-50% -100%}
.c-tip b{display:block;color:var(--ink);font-size:13px}
.c-tip span{color:var(--muted)}
`
	chartTokensLight = `--series:#6a4ce2;--unused:#d9466f;--surface:#faf9f5;`
	chartTokensDark  = `--series:#9887ef;--unused:#e24d8b;--surface:#2a2723;`
)

// chartJS adds the crosshair and tooltip. Values are inserted with textContent.
const chartJS = `
(()=>{
const whenFmt=new Intl.DateTimeFormat(undefined,{weekday:'short',day:'numeric',month:'short',hour:'2-digit',minute:'2-digit'});
const dayFmt=new Intl.DateTimeFormat(undefined,{weekday:'short'});
// Day labels at the viewer's local midnights, replacing the ones placed by the proxy.
function days(svg){
  const t0=Number(svg.dataset.t0),t1=Number(svg.dataset.t1),x0=Number(svg.dataset.x0),w=Number(svg.dataset.w);
  if(!(t1>t0)||!(w>0))return;
  svg.querySelectorAll('.c-day').forEach(n=>n.remove());
  const ns='http://www.w3.org/2000/svg';const d=new Date(t0);d.setHours(24,0,0,0);
  for(;d.getTime()<t1;d.setDate(d.getDate()+1)){
    const x=x0+(d.getTime()-t0)/(t1-t0)*w;if(x-x0<14||x0+w-x<14)continue;
    const t=document.createElementNS(ns,'text');t.setAttribute('class','c-tick c-day');t.setAttribute('x',x.toFixed(1));
    t.setAttribute('y',svg.dataset.ty);t.setAttribute('text-anchor','middle');t.textContent=dayFmt.format(d);svg.append(t);}
}
function bind(svg){
  if(svg.dataset.bound)return;svg.dataset.bound='1';
  days(svg);
  let pts;try{pts=JSON.parse(svg.dataset.points||'[]');}catch{pts=[];}
  if(!pts.length)return;
  const card=svg.closest('.chart-card');const tip=document.createElement('div');tip.className='c-tip';tip.hidden=true;
  const b=document.createElement('b');const s=document.createElement('span');tip.append(b,s);card.append(tip);
  const ns='http://www.w3.org/2000/svg';
  const hair=document.createElementNS(ns,'line');hair.setAttribute('class','c-hair');hair.setAttribute('y1',svg.dataset.top);hair.setAttribute('y2',svg.dataset.bottom);
  const dot=document.createElementNS(ns,'circle');dot.setAttribute('class','c-dot');dot.setAttribute('r','4');
  hair.style.display=dot.style.display='none';svg.append(hair,dot);
  let idx=pts.length-1;
  function show(i){idx=Math.max(0,Math.min(pts.length-1,i));const p=pts[idx];
    hair.setAttribute('x1',p[0]);hair.setAttribute('x2',p[0]);dot.setAttribute('cx',p[0]);dot.setAttribute('cy',p[1]);
    hair.style.display=dot.style.display='';
    b.textContent=p[2];s.textContent=whenFmt.format(new Date(p[3]));tip.hidden=false;
    const box=svg.getBoundingClientRect(),cb=card.getBoundingClientRect(),vb=svg.viewBox.baseVal;
    const x=box.left-cb.left+p[0]/vb.width*box.width,y=box.top-cb.top+p[1]/vb.height*box.height-10;
    tip.style.left=Math.max(60,Math.min(cb.width-60,x))+'px';tip.style.top=y+'px';}
  function hide(){hair.style.display=dot.style.display='none';tip.hidden=true;}
  function nearest(clientX){const box=svg.getBoundingClientRect(),vb=svg.viewBox.baseVal;const x=(clientX-box.left)/box.width*vb.width;
    let best=0,d=Infinity;pts.forEach((p,i)=>{const dd=Math.abs(p[0]-x);if(dd<d){d=dd;best=i;}});return best;}
  svg.addEventListener('pointermove',e=>show(nearest(e.clientX)));
  svg.addEventListener('pointerleave',hide);
  svg.addEventListener('focus',()=>show(idx));
  svg.addEventListener('blur',hide);
  svg.addEventListener('keydown',e=>{if(e.key==='ArrowLeft'){show(idx-1);e.preventDefault();}
    else if(e.key==='ArrowRight'){show(idx+1);e.preventDefault();}
    else if(e.key==='Home'){show(0);e.preventDefault();}else if(e.key==='End'){show(pts.length-1);e.preventDefault();}
    else if(e.key==='Escape'){hide();}});
}
window.bindCharts=()=>document.querySelectorAll('svg[data-points]').forEach(bind);
window.bindCharts();
})();
`

// chartInput is everything one account's chart needs.
type chartInput struct {
	Label  string
	Window usageWindow // current reading of the weekly window
	Points []point     // stored readings of this cycle
	Pace   pace
	Now    time.Time
}

func renderChart(in chartInput) string {
	reset := in.Window.ResetsAt
	start := reset.Add(-weekLength)
	span := reset.Sub(start).Seconds()
	plotW, plotH := chartW-chartL-chartR, chartH-chartT-chartB
	x := func(t time.Time) float64 {
		f := t.Sub(start).Seconds() / span
		return chartL + math.Max(0, math.Min(1, f))*plotW
	}
	y := func(pct float64) float64 { return chartT + (1-math.Max(0, math.Min(100, pct))/100)*plotH }
	baseY := y(0)

	// Readings inside the cycle plus the current one, oldest first.
	var pts []point
	for _, p := range in.Points {
		if !p.At.Before(start) && !p.At.After(in.Now) {
			pts = append(pts, p)
		}
	}
	if len(pts) == 0 || in.Now.Sub(pts[len(pts)-1].At) > time.Minute {
		pts = append(pts, point{At: in.Now, Pct: in.Window.Utilization})
	}

	var b bytes.Buffer
	// Times in the description are relative, so they read right in any time zone.
	desc := fmt.Sprintf("Weekly usage for %s: %.0f%% used, resets in %s.", in.Label, in.Window.Utilization, humanDuration(reset.Sub(in.Now)))
	if in.Pace.Full {
		desc += " The limit is reached."
	} else if in.Pace.Known {
		if !in.Pace.RunsOutAt.IsZero() {
			desc += " At the current pace the limit is reached in " + humanDuration(in.Pace.RunsOutAt.Sub(in.Now)) + "."
		} else {
			desc += fmt.Sprintf(" At the current pace about %.0f%% will be unused.", in.Pace.Unused)
		}
	}
	type hoverPoint [4]any
	hover := make([]hoverPoint, 0, len(pts))
	for _, p := range pts {
		// The time is sent as Unix milliseconds; the script formats it in the viewer's time zone.
		hover = append(hover, hoverPoint{round1(x(p.At)), round1(y(p.Pct)), fmt.Sprintf("%.0f%% used", p.Pct), p.At.UnixMilli()})
	}
	hoverJSON, _ := json.Marshal(hover)
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" tabindex="0" aria-label="%s" data-points="%s" data-top="%.0f" data-bottom="%.0f" data-x0="%.1f" data-w="%.1f" data-ty="%.1f" data-t0="%d" data-t1="%d">`,
		chartW, chartH, html.EscapeString(desc), html.EscapeString(string(hoverJSON)), chartT, baseY, chartL, plotW, chartH-6, start.UnixMilli(), reset.UnixMilli())

	// Grid and y ticks.
	for _, pct := range []float64{0, 50, 100} {
		fmt.Fprintf(&b, `<line class="c-grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, chartL, chartL+plotW, y(pct), y(pct))
		fmt.Fprintf(&b, `<text class="c-tick" x="%.1f" y="%.1f" text-anchor="end" dominant-baseline="middle">%.0f%%</text>`, chartL-6, y(pct), pct)
	}
	// Day labels at the proxy's local midnights; the script moves them to the viewer's.
	ls := start.Local()
	for day := time.Date(ls.Year(), ls.Month(), ls.Day()+1, 0, 0, 0, 0, time.Local); day.Before(reset); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, time.Local) {
		if x(day)-chartL < 14 || chartL+plotW-x(day) < 14 {
			continue
		}
		fmt.Fprintf(&b, `<text class="c-tick c-day" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x(day), chartH-6, day.Format("Mon"))
	}

	// Measured usage: area wash and line.
	var line strings.Builder
	for i, p := range pts {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(p.At), y(p.Pct))
	}
	first, last := pts[0], pts[len(pts)-1]
	fmt.Fprintf(&b, `<path class="c-area" d="%sL%.1f %.1f L%.1f %.1f Z"/>`, line.String(), x(last.At), baseY, x(first.At), baseY)
	fmt.Fprintf(&b, `<path class="c-line" d="%s"/>`, strings.TrimSpace(line.String()))

	// "now" marker.
	nowX := x(in.Now)
	fmt.Fprintf(&b, `<line class="c-now" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, nowX, nowX, chartT-2, baseY)
	fmt.Fprintf(&b, `<text class="c-sub" x="%.1f" y="%.1f" text-anchor="middle">now</text>`, nowX, chartT-5)

	// Projection to the reset at the current pace.
	endX := x(reset)
	if in.Pace.Known && !in.Pace.Full {
		projEndX, projEndY := endX, y(in.Pace.Projected)
		if !in.Pace.RunsOutAt.IsZero() {
			projEndX = x(in.Pace.RunsOutAt)
		}
		fmt.Fprintf(&b, `<path class="c-proj" d="M%.1f %.1f L%.1f %.1f"/>`, nowX, y(in.Window.Utilization), projEndX, projEndY)
		if in.Pace.Unused >= 0.5 {
			// Bracket at the reset between the projected end and the limit: the part lost.
			fmt.Fprintf(&b, `<rect class="c-unused" x="%.1f" y="%.1f" width="8" height="%.1f" rx="4"/>`, endX-4, y(100), projEndY-y(100))
			fmt.Fprintf(&b, `<line class="c-unused-edge" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, endX, endX, y(100), projEndY)
			midY := (y(100) + projEndY) / 2
			fmt.Fprintf(&b, `<text class="c-label" x="%.1f" y="%.1f">%.0f%%</text>`, endX+8, midY-1, in.Pace.Unused)
			fmt.Fprintf(&b, `<text class="c-sub" x="%.1f" y="%.1f">unused</text>`, endX+8, midY+11)
		} else {
			fmt.Fprintf(&b, `<text class="c-label" x="%.1f" y="%.1f" dominant-baseline="middle">all used</text>`, endX+6, y(100))
		}
	}
	// Current value: end dot with surface ring, labelled.
	fmt.Fprintf(&b, `<circle class="c-dot" cx="%.1f" cy="%.1f" r="4"/>`, nowX, y(in.Window.Utilization))
	labelY := y(in.Window.Utilization) - 9
	if labelY < chartT+8 {
		labelY = y(in.Window.Utilization) + 16
	}
	fmt.Fprintf(&b, `<text class="c-label" x="%.1f" y="%.1f" text-anchor="end">%.0f%%</text>`, nowX-6, labelY, in.Window.Utilization)
	b.WriteString(`</svg>`)
	return b.String()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// paceSummary is the one-line reading of a pace, shared by the hero and the charts.
func paceSummary(p pace, now time.Time) (headline, detail string) {
	if !p.Known {
		return "", fmt.Sprintf("Needs about %.0f hours of readings from the last day to estimate the pace.", paceMinSpan.Hours())
	}
	if p.Full {
		return "Limit reached", "Nothing more until the reset."
	}
	rate := fmt.Sprintf("%.1f points per hour", p.PerHour)
	if !p.RunsOutAt.IsZero() {
		return "Runs out in " + humanDuration(p.RunsOutAt.Sub(now)), "At " + rate + ", before the reset."
	}
	if p.Unused < 0.5 {
		return "On pace to use it all", "At " + rate + "."
	}
	return fmt.Sprintf("%.0f%% would go unused", p.Unused), fmt.Sprintf("At %s. Using it all needs %.1f.", rate, p.NeededPerHour)
}
