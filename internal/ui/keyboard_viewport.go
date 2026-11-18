package ui

// keyboardViewportJS is THE ONE reading of how much of the screen a soft
// keyboard covers, spliced into each script that needs it (each runs in its
// own function scope, so each gets its own copy of the two functions, from
// this one text): the Rendered view's pinned composer (composerPinScript), the
// bar's keyboard mark (switcherKeyboardJS) and the switcher dock (dockJS).
//
//   - kbWin() is the window whose viewport a keyboard shrinks: the TOP window
//     inside the view switch's frame (view_swap.go). The frame is laid over the
//     whole viewport, but a keyboard that only overlays the page (iOS) shrinks
//     the top window's visual viewport, never the frame's. Elsewhere, window.
//   - kbMeasure(vw) is vw's viewport now, or null under a PINCH ZOOM (a
//     visualViewport.scale other than 1): a zoom shrinks the visual viewport
//     without the page being any shorter, so it is not a keyboard, and each
//     caller decides what a pinch means for it. Otherwise:
//     h      the visual viewport's height (innerHeight without one);
//     bottom the visual viewport's bottom in layout-viewport pixels;
//     kb     the layout viewport below that bottom: 0 where the keyboard
//     resizes the page (interactive-widget=resizes-content, the
//     Android shell), its height where it only overlays it.
const keyboardViewportJS = `
  function kbWin() {
    var vw = window;
    try { if (window.frameElement && window.frameElement.hasAttribute('` + ViewFrameAttr + `')) vw = window.parent; } catch (e) {}
    return vw;
  }
  function kbMeasure(vw) {
    var vv = vw.visualViewport, bottom = vw.innerHeight;
    if (vv) {
      if (Math.abs(vv.scale - 1) > 0.001) return null;
      bottom = Math.min(vw.innerHeight, vv.height + vv.offsetTop);
    }
    return { h: vv ? vv.height : vw.innerHeight, bottom: bottom, kb: Math.max(0, Math.round(vw.innerHeight - bottom)) };
  }
`
