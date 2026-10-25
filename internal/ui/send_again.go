package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// SendAgainField is the form field that says a send IS the person's explicit
// "Send again" of a message that may already have been delivered: the server
// refuses the same text to the same place without it (internal/api
// sendNonces.hold).
const SendAgainField = "again"

// SendAgainView is SendAgainPage's content.
type SendAgainView struct {
	// Action is the route the page posts to: the Send, Queue or Send now it
	// answers, once more, with SendAgainField=1.
	Action string
	// Fields are what that post carries besides: the address, the text, a
	// fresh nonce. Nothing for a Send now (its route names the message).
	Fields []ComposerField
	// Why is the refusal, in the server's words.
	Why string
	// Text is the message, shown so the person can tell which one it is.
	Text string
	// Button is the button's text ("Send again", "Queue it anyway"…).
	Button string
	// BackHref is where Cancel goes: the view it came from.
	BackHref string
}

// SendAgainPage is a whole document with one form: the browser's own answer
// to a send refused because the same message may already have been delivered,
// when no script ran. 🔴 WITHOUT IT A PAGE WITH NO SCRIPT IS LOCKED OUT: the
// refusal says "use Send again", and Send again is a button the composer's
// script draws (audit #275 round 1, Y2). This is that button, as a form.
func SendAgainPage(v SendAgainView) g.Node {
	fields := make([]g.Node, 0, len(v.Fields)+1)
	for _, f := range v.Fields {
		fields = append(fields, Input(Type("hidden"), Name(f.Name), Value(f.Value)))
	}
	fields = append(fields, Input(Type("hidden"), Name(SendAgainField), Value("1")))
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Send it again? · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				faroHead(),
			),
			Body(
				Class("flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased"),
				Form(
					ID("send-again"),
					Method("post"),
					Action(v.Action),
					Class("flex w-full max-w-sm flex-col gap-4 rounded-2xl bg-s1 px-5 py-6 ring-1 ring-line"),
					g.Group(fields),
					H1(Class("text-base font-semibold text-fg"), g.Text("Send it again?")),
					P(Class("text-sm text-fg2"), g.Text(v.Why)),
					P(Class("text-sm text-fg2"), g.Text(v.Text)),
					Div(
						Class("flex gap-2"),
						A(Href(v.BackHref),
							Class("press inline-flex min-h-[44px] flex-1 items-center justify-center rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
							g.Text("Cancel")),
						Button(Type("submit"), ID("send-again-submit"),
							Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
							g.Text(v.Button)),
					),
				),
				pwaChrome(),
			),
		),
	)
}
