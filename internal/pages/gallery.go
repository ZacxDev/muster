package pages

import "strconv"

// ---------------------------------------------------------------------------
// THE GALLERY BLOCK (docs/pages.md#gallery): a grid of an author's images to
// compare on a phone, each opening full size, and — with pick — a Pick toggle
// per image and ONE save of the picks.
//
// renderGallery (render.go) draws the grid; everything else is the SDK's,
// written here (galleryJS), and runs in the content frame like every other
// part of the SDK:
//
//   - The full-size viewer is a dialog in the frame: a horizontal
//     scroll-snap track (the browser's own momentum and snap on a swipe),
//     arrows and ← / → on a desktop, Esc and × close, a counter and the
//     caption, pinch and double-tap zoom (a pan while zoomed; the track stops
//     scrolling while zoomed), the image either side loaded as one is shown.
//   - 🔴 BACK CLOSES IT, AND THE HISTORY ENTRY IS THE SHELL'S. The frame's
//     sandbox gives it an opaque origin, and an entry a subframe pushes is
//     invisible to an Android WebView's own back handling (its
//     doUpdateVisitedHistory reports the main frame only, so the app's back
//     callback can stay disabled and Back leaves the app). So the SDK only
//     TELLS the shell, on its private port: {op:"viewer", open}. The shell —
//     the top document on muster's origin — pushes one entry on open and
//     pops it on a close by ×, Esc or a tap outside; a Back that pops it
//     answers {op:"back"} on the port and the viewer closes. A page's own
//     script cannot reach the port, so it cannot push entries this way.
//   - The picks are local until Save: a tap toggles at once and sends
//     nothing. The frame has no storage (an opaque origin), so the SDK keeps
//     them in the SHELL's sessionStorage over the port ({op:"keep"} /
//     {op:"recall"}, keyed by the version and the gallery's id, under the
//     shell's own path): a reload keeps unsaved picks, per tab and per page
//     version. The shell keeps only the keys the server named for the
//     version it rendered (KeepKeys), since a page's script can add
//     gallery-shaped sections this script would keep for. Save sends
//     {picks} once through muster.submit's own path — the owner's "Send as
//     you?" confirm, by_owner, a link's viewer's response — and a second Save waits for the first's answer.
// ---------------------------------------------------------------------------

// HasGallery reports whether blocks hold a gallery (so the content document
// carries the gallery's script and styles).
func HasGallery(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type == BlockGallery {
			return true
		}
	}
	return false
}

// KeepKeys is the keys the shell keeps v's unsaved picks under for the frame
// (ShellView.Keeps): "v<version>:<id>" for each gallery with a pick, the key
// galleryJS builds. The shell refuses a keep or recall of any other key, so a
// page's script cannot add keys by forging gallery sections.
func KeepKeys(v Version) []string {
	out := []string{}
	for _, b := range v.Blocks {
		if b.Type == BlockGallery && b.Pick != nil {
			out = append(out, "v"+strconv.Itoa(v.Version)+":"+b.ID)
		}
	}
	return out
}

// galleryJS is the SDK's gallery part for version v, inside the SDK's own
// function (it uses send, viewer, onBack and the saved D, W). A version with
// no gallery gets a gal that does nothing.
//
// 🔴 IT USES NO PROMISE AND NO .then (the SDK's rule: a page's script may
// have replaced them) and builds every message to the shell through send and
// viewer, which build their own object literals.
func galleryJS(v Version) string {
	if !HasGallery(v.Blocks) {
		return `function gal(){}`
	}
	return `// THE GALLERY (gallery.go).
var VER=` + strconv.Itoa(v.Version) + `,RM=W.matchMedia?W.matchMedia("(prefers-reduced-motion: reduce)"):null;
function gal(){var l=D.querySelectorAll('section[data-muster-kind="gallery"]');for(var i=0;i<l.length;i++)galOne(l[i]);}
function galOne(sec){
 var id=sec.getAttribute("data-muster-component"),mode=sec.getAttribute("data-muster-pick")||"",req=sec.hasAttribute("data-muster-required"),
  lab=sec.querySelector(".mp-label"),title=lab?lab.textContent:"Gallery",key="v"+VER+":"+id,
  lis=sec.querySelectorAll("li[data-muster-item]"),items=[],picked=[],saved=null,busy=false,touched=false,i;
 for(i=0;i<lis.length;i++){var li=lis[i];items.push({li:li,a:li.getAttribute("data-muster-asset-id")||"",c:li.getAttribute("data-muster-caption")||"",
  name:li.getAttribute("data-muster-name")||"",img:li.querySelector("img"),open:li.querySelector(".mp-gallery-open"),pick:li.querySelector(".mp-gallery-pick")});}
 var n=items.length,bar=sec.querySelector(".mp-gallery-bar"),count=bar&&bar.querySelector("[data-muster-picked]"),
  saveBtn=bar&&bar.querySelector("[data-muster-save]"),status=bar&&bar.querySelector("[data-muster-status]");
 var vw=null,cur=0,opener=null,z={s:1,x:0,y:0},pinch=null,pan=null,tap=null,lastTap=null,mpan=null,aim=-1,aimAt=0;
 function has(k){return picked.indexOf(k)>=0;}
 function same(a,b){if(!a||!b||a.length!==b.length)return false;for(var j=0;j<a.length;j++)if(a[j]!==b[j])return false;return true;}
 function say(t,bad){if(status){status.textContent=t;status.className="mp-status"+(t?(bad?" mp-bad":" mp-good"):"");}}
 function paintBtn(b,on){if(!b)return;b.setAttribute("aria-pressed",on?"true":"false");b.textContent=on?"Picked ✓":"Pick";}
 function paint(){
  for(var j=0;j<n;j++){var on=has(j);items[j].li.classList.toggle("mp-picked",on);paintBtn(items[j].pick,on);}
  if(vw&&vw.pk)paintBtn(vw.pk,has(cur));
  if(count)count.textContent=picked.length+" picked";
  if(!busy){if(saved&&same(picked,saved))say("Saved.");else if(saved)say("Changed since you saved: Save again to send these picks.");else say("");}
 }
 function keep(){send("keep",key,{p:picked.slice(),s:saved?saved.slice():null},function(){});}
 function toggle(k){
  if(!mode||k<0||k>=n)return;touched=true;
  if(mode==="several"){if(has(k))picked.splice(picked.indexOf(k),1);else{picked.push(k);picked.sort(function(a,b){return a-b;});}}
  else picked=has(k)?[]:[k];
  paint();keep();
 }
 // clean is a recalled list as the gallery takes it: whole indices of its
 // items, each once, in order; one at most for a single pick.
 function clean(x){
  if(!Array.isArray(x))return null;var o=[];
  for(var j=0;j<x.length;j++){var k=x[j];if(typeof k==="number"&&k%1===0&&k>=0&&k<n&&o.indexOf(k)<0)o.push(k);}
  o.sort(function(a,b){return a-b;});if(mode!=="several"&&o.length>1)o=o.slice(0,1);return o;
 }
 // save sends the picks ONCE: while one is on its way (the owner's confirm
 // included), another tap of Save does nothing.
 function save(){
  if(busy||!mode)return;
  if(req&&!picked.length){say(mode==="several"?"Pick at least one first.":"Pick one first.",true);return;}
  busy=true;if(saveBtn)saveBtn.disabled=true;say("Sending…");
  var mine=picked.slice(),data=[];
  for(var j=0;j<mine.length;j++){var it=items[mine[j]],p={index:mine[j],asset:"asset:"+it.a};if(it.c)p.caption=it.c;data.push(p);}
  send("submit",id,{picks:data},function(ok,x){
   busy=false;if(saveBtn)saveBtn.disabled=false;
   if(ok){saved=mine;keep();paint();}else{paint();say(x||"It could not be sent.",true);}
  });
 }
 function el(tag,cls,parent){var e=D.createElement(tag);if(cls)e.className=cls;if(parent)parent.appendChild(e);return e;}
 function btn(cls,label,text,parent){var b=el("button",cls,parent);b.type="button";if(label)b.setAttribute("aria-label",label);b.textContent=text;return b;}
 function moved(){return !(RM&&RM.matches);}
 function im(){return vw.slides[cur].im;}
 function apply(){
  var m=im();m.style.transform=z.s>1?"translate("+z.x+"px,"+z.y+"px) scale("+z.s+")":"";
  m.setAttribute("data-muster-zoom",String(Math.round(z.s*100)/100));vw.d.classList.toggle("mp-zoomed",z.s>1);
 }
 // drawn is the size the image is drawn at, unzoomed: its img fills the slide
 // (so a small image is enlarged) and object-fit:contain letterboxes it, so
 // the picture is the natural size scaled to fit the box. 0 x 0 until loaded.
 function drawn(m){var w=m.clientWidth,h=m.clientHeight,nw=m.naturalWidth,nh=m.naturalHeight;if(!nw||!nh)return {w:0,h:0};var s=Math.min(w/nw,h/nh);return {w:nw*s,h:nh*s};}
 // clamp keeps a zoomed picture over the viewer: it pans only as far as its
 // own drawn edges reach past the track's, and stays centred on an axis where
 // it is narrower than the track.
 function clamp(){var w=vw.tr.clientWidth,h=vw.tr.clientHeight,d=drawn(im()),mx=Math.max(0,(z.s*d.w-w)/2),my=Math.max(0,(z.s*d.h-h)/2);z.x=Math.max(-mx,Math.min(mx,z.x));z.y=Math.max(-my,Math.min(my,z.y));}
 // beside is whether a tap at (x, y) misses the picture: on its slide, or on
 // the img's letterbox around the drawn picture.
 function beside(t,x,y){
  if(!t||!t.classList)return false;if(t.classList.contains("mp-viewer-slide"))return true;
  if(t!==im())return false;var r=t.getBoundingClientRect(),d=drawn(t);
  return Math.abs(x-(r.left+r.width/2))>d.w/2||Math.abs(y-(r.top+r.height/2))>d.h/2;
 }
 function unzoom(){if(!vw)return;if(z.s!==1||z.x||z.y){z={s:1,x:0,y:0};apply();}pinch=null;pan=null;mpan=null;}
 function rel(x,y){var r=vw.tr.getBoundingClientRect();return {x:x-(r.left+r.width/2),y:y-(r.top+r.height/2)};}
 function zoomTo(s,px,py){
  var m=rel(px,py);s=Math.max(1,Math.min(5,s));
  z.x=m.x-s*(m.x-z.x)/z.s;z.y=m.y-s*(m.y-z.y)/z.s;z.s=s;if(s===1){z.x=0;z.y=0;}clamp();apply();
 }
 function prep(j){
  for(var k=Math.max(0,j-1);k<=Math.min(n-1,j+1);k++){var sl=vw.slides[k];if(!sl.set&&items[k].img){sl.im.setAttribute("src",items[k].img.getAttribute("src")||"");sl.set=true;}}
 }
 function setCur(j){
  if(j!==cur)unzoom();cur=j;prep(j);
  vw.cnt.textContent=(j+1)+" / "+n;vw.cap.textContent=items[j].c;vw.cap.hidden=!items[j].c;
  vw.pv.setAttribute("aria-disabled",j===0?"true":"false");vw.nx.setAttribute("aria-disabled",j===n-1?"true":"false");
  if(vw.pk){vw.pk.setAttribute("aria-label","Pick "+items[j].name);paintBtn(vw.pk,has(j));}
 }
 // go moves to item j by the arrows or a key: the item is current at once, and
 // the track's scroll on its way there (aim) does not make the items it
 // passes current.
 function go(j){if(!vw||j<0||j>=n)return;unzoom();setCur(j);aim=j;aimAt=Date.now();vw.tr.scrollTo({left:j*vw.tr.clientWidth,behavior:moved()?"smooth":"auto"});}
 function focusables(){var l=vw.d.querySelectorAll("button"),o=[];for(var j=0;j<l.length;j++)if(!l[j].hidden&&!l[j].disabled&&l[j].getClientRects().length)o.push(l[j]);return o;}
 function keys(e){
  if(!vw||vw.d.hidden)return;var k=e.key;
  if(k==="Escape"){e.preventDefault();e.stopPropagation();shut(false);}
  else if(k==="ArrowLeft"){e.preventDefault();go(cur-1);}
  else if(k==="ArrowRight"){e.preventDefault();go(cur+1);}
  else if(k==="Tab"){
   var f=focusables();if(!f.length)return;var ix=f.indexOf(D.activeElement);
   if(e.shiftKey){if(ix<=0){e.preventDefault();f[f.length-1].focus();}}
   else if(ix<0||ix===f.length-1){e.preventDefault();f[0].focus();}
  }
 }
 function trap(e){if(vw&&!vw.d.hidden&&!vw.d.contains(e.target))vw.x.focus();}
 function build(){
  var d=el("div","mp-viewer");d.setAttribute("role","dialog");d.setAttribute("aria-modal","true");d.setAttribute("aria-label",title+": full size");
  d.setAttribute("data-muster-viewer",id);d.hidden=true;
  var top=el("div","mp-viewer-top",d),cnt=el("span","mp-viewer-count",top);cnt.setAttribute("aria-live","polite");
  var x=btn("mp-viewer-close","Close full size","×",top),tr=el("div","mp-viewer-track",d),slides=[];
  for(var j=0;j<n;j++){var sl=el("div","mp-viewer-slide",tr),m=el("img","",sl);m.alt=items[j].img?items[j].img.alt:"";m.draggable=false;slides.push({sl:sl,im:m,set:false});}
  var pv=btn("mp-viewer-prev","Previous image","‹",d),nx=btn("mp-viewer-next","Next image","›",d);
  var bot=el("div","mp-viewer-bottom",d),cap=el("p","mp-viewer-cap",bot),pk=null;
  if(mode){pk=btn("mp-viewer-pick mp-secondary","","Pick",bot);pk.addEventListener("click",function(){toggle(cur);});}
  vw={d:d,cnt:cnt,x:x,tr:tr,slides:slides,pv:pv,nx:nx,cap:cap,pk:pk};
  x.addEventListener("click",function(){shut(false);});
  pv.addEventListener("click",function(){go(cur-1);});nx.addEventListener("click",function(){go(cur+1);});
  tr.addEventListener("scroll",function(){
   if(z.s>1)return;var w=tr.clientWidth;if(!w)return;var j=Math.round(tr.scrollLeft/w);
   if(aim>=0){if(j!==aim&&Date.now()-aimAt<1500)return;aim=-1;}
   if(j!==cur&&j>=0&&j<n)setCur(j);
  },{passive:true});
  // A tap beside the picture (its slide, or the img's letterbox) closes,
  // unless zoomed.
  tr.addEventListener("click",function(e){if(z.s===1&&beside(e.target,e.clientX,e.clientY))shut(false);});
  tr.addEventListener("dblclick",function(e){if(z.s>1)unzoom();else zoomTo(2.5,e.clientX,e.clientY);});
  tr.addEventListener("touchstart",function(e){
   var t=e.touches;aim=-1;
   if(t.length===2){e.preventDefault();var m=rel((t[0].clientX+t[1].clientX)/2,(t[0].clientY+t[1].clientY)/2);
    pinch={d:Math.hypot(t[0].clientX-t[1].clientX,t[0].clientY-t[1].clientY)||1,s:z.s,x:z.x,y:z.y,mx:m.x,my:m.y};pan=null;tap=null;}
   else if(t.length===1){tap={x:t[0].clientX,y:t[0].clientY};if(z.s>1){e.preventDefault();pan={x:t[0].clientX,y:t[0].clientY,ox:z.x,oy:z.y};}}
  },{passive:false});
  tr.addEventListener("touchmove",function(e){
   var t=e.touches;
   if(pinch&&t.length===2){e.preventDefault();var m=rel((t[0].clientX+t[1].clientX)/2,(t[0].clientY+t[1].clientY)/2),
    s=Math.max(1,Math.min(5,pinch.s*Math.hypot(t[0].clientX-t[1].clientX,t[0].clientY-t[1].clientY)/pinch.d));
    z.x=m.x-s*(pinch.mx-pinch.x)/pinch.s;z.y=m.y-s*(pinch.my-pinch.y)/pinch.s;z.s=s;clamp();apply();}
   else if(pan&&t.length===1){e.preventDefault();z.x=pan.ox+t[0].clientX-pan.x;z.y=pan.oy+t[0].clientY-pan.y;clamp();apply();}
   if(tap&&t.length===1&&Math.abs(t[0].clientX-tap.x)+Math.abs(t[0].clientY-tap.y)>12)tap=null;
  },{passive:false});
  tr.addEventListener("touchend",function(e){
   if(pinch&&e.touches.length<2){pinch=null;if(z.s<1.05)unzoom();}
   if(pan&&!e.touches.length)pan=null;
   if(tap&&!e.touches.length&&e.changedTouches.length===1){
    var c=e.changedTouches[0],now=Date.now();
    if(lastTap&&now-lastTap.at<320&&Math.abs(c.clientX-lastTap.x)+Math.abs(c.clientY-lastTap.y)<30){
     e.preventDefault();lastTap=null;if(z.s>1)unzoom();else zoomTo(2.5,c.clientX,c.clientY);}
    else lastTap={x:c.clientX,y:c.clientY,at:now};
   }
   tap=null;
  },{passive:false});
  // A mouse drags a zoomed image (a touch pans in touchmove).
  tr.addEventListener("pointerdown",function(e){if(e.pointerType==="mouse"&&z.s>1){e.preventDefault();mpan={x:e.clientX,y:e.clientY,ox:z.x,oy:z.y};}});
  W.addEventListener("pointermove",function(e){if(mpan){z.x=mpan.ox+e.clientX-mpan.x;z.y=mpan.oy+e.clientY-mpan.y;clamp();apply();}});
  W.addEventListener("pointerup",function(){mpan=null;});
  D.body.appendChild(d);
 }
 function openAt(k,from){
  if(!vw)build();if(!vw.d.hidden)return;
  opener=from;vw.d.hidden=false;D.documentElement.classList.add("mp-viewer-open");
  cur=k;unzoom();setCur(k);vw.tr.scrollLeft=k*vw.tr.clientWidth;vw.x.focus();
  D.addEventListener("keydown",keys,true);D.addEventListener("focusin",trap,true);
  onBack=function(){shut(true);};viewer(true);
 }
 // shut closes the viewer: by Back (the shell already popped its entry), or
 // by ×, Esc or a tap outside, when the shell pops the entry itself.
 function shut(byBack){
  if(!vw||vw.d.hidden)return;
  unzoom();vw.d.hidden=true;D.documentElement.classList.remove("mp-viewer-open");
  D.removeEventListener("keydown",keys,true);D.removeEventListener("focusin",trap,true);
  if(!byBack){onBack=null;viewer(false);}
  if(opener)opener.focus();opener=null;
 }
 for(i=0;i<n;i++)(function(k){if(items[k].open)items[k].open.addEventListener("click",function(){openAt(k,items[k].open);});})(i);
 if(mode){
  if(saveBtn)saveBtn.addEventListener("click",save);
  for(i=0;i<n;i++)(function(k){if(items[k].pick)items[k].pick.addEventListener("click",function(){toggle(k);});})(i);
  send("recall",key,null,function(ok,x){
   if(!ok||touched||!x||typeof x!=="object")return;
   var p=clean(x.p),sv=x.s===null?null:clean(x.s);if(!p)return;
   picked=p;saved=sv;paint();
  });
 }
}`
}

// galleryCSS is the gallery's styles, carried by a version with a gallery
// (Content): the palette's roles, as contentCSS names them. The viewer's top
// row starts below the shell's header (shellHeaderPx), which is laid over the
// frame.
const galleryCSS = `.mp-gallery .mp-label{margin:0 0 10px}
.mp-gallery-group{margin:18px 0 8px;font-size:1em;font-weight:600;color:var(--fg2)}
.mp-gallery-grid{list-style:none;margin:0;padding:0;display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:12px}
.mp-gallery[data-muster-layout=rows] .mp-gallery-grid{grid-template-columns:1fr}
.mp-gallery-item{position:relative;display:flex;flex-direction:column;gap:6px;min-width:0}
.mp-gallery-open{position:relative;display:block;width:100%;min-height:0;padding:0;border:1px solid var(--line);border-radius:10px;background:var(--line);overflow:hidden;align-self:stretch;cursor:zoom-in}
.mp-gallery-open img{display:block;width:100%;aspect-ratio:1/1;height:auto;object-fit:cover;border-radius:0}
.mp-gallery[data-muster-layout=rows] .mp-gallery-open img{aspect-ratio:auto;max-height:80vh;object-fit:contain}
.mp-gallery-open:focus-visible,.mp-viewer button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.mp-gallery-badge{display:none;position:absolute;top:6px;right:6px;width:28px;height:28px;border-radius:50%;background:var(--accent);color:var(--on-accent);font-weight:700;line-height:28px;text-align:center}
.mp-picked .mp-gallery-badge{display:block}.mp-picked .mp-gallery-open{border-color:var(--accent);box-shadow:0 0 0 2px var(--accent)}
.mp-gallery-cap{margin:0;font-size:14px;color:var(--fg2);overflow-wrap:anywhere}
.mp-gallery-pick{align-self:stretch}.mp-gallery button,.mp-viewer button{-webkit-tap-highlight-color:transparent}
.mp-gallery-pick[aria-pressed=true],.mp-viewer-pick[aria-pressed=true]{background:var(--accent);color:var(--on-accent);border-color:var(--accent)}
.mp-gallery-bar{position:sticky;bottom:0;z-index:1;display:flex;flex-wrap:wrap;align-items:center;gap:6px 10px;margin-top:14px;padding:10px 14px calc(10px + env(safe-area-inset-bottom));
background:var(--bg);border:1px solid var(--line);border-radius:12px}
.mp-gallery-bar [data-muster-picked]{font-weight:600}.mp-gallery-bar .mp-status{flex-basis:100%}.mp-gallery-bar .mp-status:empty{display:none}
html.mp-viewer-open{overflow:hidden}
.mp-viewer{position:fixed;inset:0;z-index:10;display:flex;flex-direction:column;background:var(--bg);color:var(--fg)}
.mp-viewer[hidden]{display:none}
.mp-viewer button{align-self:auto}
.mp-viewer-top{flex:none;display:flex;align-items:center;justify-content:space-between;gap:8px;padding:` + shellHeaderPx + `px 8px 4px 16px}
.mp-viewer-count{font-variant-numeric:tabular-nums;font-weight:600}
.mp-viewer-close{min-width:44px;padding:0 12px;font-size:24px;line-height:1;background:transparent;color:var(--fg);border:1px solid var(--line)}
.mp-viewer-track{flex:1;min-height:0;display:flex;overflow-x:auto;overflow-y:hidden;scroll-snap-type:x mandatory;overscroll-behavior:contain;touch-action:pan-x;scrollbar-width:none}
.mp-viewer-track::-webkit-scrollbar{display:none}
.mp-zoomed .mp-viewer-track{overflow:hidden;touch-action:none}
.mp-viewer-slide{flex:0 0 100%;width:100%;height:100%;scroll-snap-align:center;scroll-snap-stop:always;display:flex;align-items:center;justify-content:center;overflow:hidden}
.mp-viewer-slide img{width:100%;height:100%;object-fit:contain;transform-origin:center;user-select:none;-webkit-user-select:none;-webkit-user-drag:none}
.mp-zoomed .mp-viewer-slide img{cursor:grab}
.mp-viewer-prev,.mp-viewer-next{position:absolute;top:50%;transform:translateY(-50%);width:48px;height:64px;min-height:0;padding:0;font-size:32px;line-height:1;background:rgb(0 0 0/.4);color:#fff;border-radius:10px}
.mp-viewer-prev{left:8px}.mp-viewer-next{right:8px}.mp-viewer [aria-disabled=true]{opacity:.3}
@media (hover:none) and (pointer:coarse){.mp-viewer-prev,.mp-viewer-next{display:none}}
.mp-viewer-bottom{flex:none;display:flex;align-items:center;gap:10px;padding:8px 16px calc(12px + env(safe-area-inset-bottom))}
.mp-viewer-cap{flex:1;margin:0;min-width:0;overflow-wrap:anywhere}.mp-viewer-cap[hidden]{display:block;visibility:hidden}
.mp-viewer-pick{flex:none;margin-left:auto}`
