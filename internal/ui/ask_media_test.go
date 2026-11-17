package ui

import (
	"strings"
	"testing"
)

// THE ASK MEDIA HELPERS render the asker's words and files: untrusted, so
// every field is escaped, only a raster image is an <img>, and an SVG — which
// runs script when a browser renders it as a document — is a download chip.

func TestAnAskImageIsAThumbnailThatOpensFullSizeAndAnyOtherFileIsAChip(t *testing.T) {
	got := AskMediaStrip([]AskFile{
		{Name: "shot.png", Type: "image/png", Size: "12 KB", URL: "/attention/asks/7/attachments/AAAAAAAAAAAAAAAAAAAAAA", Image: true},
		{Name: "report.pdf", Type: "application/pdf", Size: "1.2 MB", URL: "/attention/asks/7/attachments/BBBBBBBBBBBBBBBBBBBBBB"},
	})
	thumb := `<a role="listitem" href="/attention/asks/7/attachments/AAAAAAAAAAAAAAAAAAAAAA" target="_blank" rel="noopener noreferrer" class="` + AskThumbClass +
		`" title="shot.png (12 KB)" data-ask-media-image><img src="/attention/asks/7/attachments/AAAAAAAAAAAAAAAAAAAAAA" alt="shot.png" loading="lazy" decoding="async" class="h-full w-full object-cover"></a>`
	if !strings.Contains(got, thumb) {
		t.Fatalf("the image is not a thumbnail linking to itself full-size:\n%s", got)
	}
	if strings.Count(got, "<img") != 1 {
		t.Fatalf("want exactly one <img> (the PNG), got:\n%s", got)
	}
	chip := `href="/attention/asks/7/attachments/BBBBBBBBBBBBBBBBBBBBBB" download="report.pdf"`
	if !strings.Contains(got, chip) || !strings.Contains(got, `data-ask-media-file`) || !strings.Contains(got, `>report.pdf</span>`) ||
		!strings.Contains(got, `>1.2 MB</span>`) {
		t.Fatalf("the PDF is not a chip with its name and size that downloads it:\n%s", got)
	}
	if AskMediaStrip(nil) != "" {
		t.Fatal("no files must draw nothing")
	}
}

func TestAnSVGAttachmentIsNeverDrawnAsAnImageOrEmbedded(t *testing.T) {
	// internal/api never marks an SVG Image (uploads.InlineImage refuses it);
	// the strip draws whatever is not Image as a chip.
	got := AskMediaStrip([]AskFile{{Name: "evil.svg", Type: "text/xml; charset=utf-8", Size: "1 KB", URL: "/attention/asks/7/attachments/CCCCCCCCCCCCCCCCCCCCCC"}})
	for _, tag := range []string{"<img", "<object", "<embed", "<iframe", "<svg onload", "<use"} {
		if strings.Contains(got, tag) {
			t.Fatalf("an SVG attachment drew %s:\n%s", tag, got)
		}
	}
	if !strings.Contains(got, `download="evil.svg"`) || !strings.Contains(got, "data-ask-media-file") {
		t.Fatalf("an SVG attachment must be a download chip:\n%s", got)
	}
}

func TestEveryFieldOfAnAttachmentIsEscaped(t *testing.T) {
	evil := `"><script>alert(1)</script>`
	got := AskMediaStrip([]AskFile{
		{Name: evil, Type: evil, Size: evil, URL: `/x"onmouseover="alert(1)`, Image: true},
		{Name: evil, Type: evil, Size: evil, URL: `/y"onmouseover="alert(1)`},
	})
	if strings.Contains(got, "<script>") || strings.Contains(got, `"onmouseover="`) {
		t.Fatalf("an attachment field broke out of its text or attribute:\n%s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("the name is not shown escaped:\n%s", got)
	}
}

func TestTheContextLineSaysWhoseWordsItIsAndEscapesThem(t *testing.T) {
	cases := []struct {
		name string
		in   AskWhy
		want string
	}{
		{"agent", AskWhy{Text: `deploy <b>blocked</b> & waiting`},
			`<p class="` + AskWhyClass + `" data-ask-why="agent"><span class="font-semibold">Why:</span> deploy &lt;b&gt;blocked&lt;/b&gt; &amp; waiting</p>`},
		{"fallback both", AskWhy{Focus: "Fix <the> build", Last: `I'll "wait"`},
			`<p class="` + AskWhyClass + `" data-ask-why="session"><span class="font-semibold">Working on:</span> Fix &lt;the&gt; build · <span class="font-semibold">Last said:</span> I&#39;ll &#34;wait&#34;</p>`},
		{"fallback focus only", AskWhy{Focus: "Fix the build"},
			`<p class="` + AskWhyClass + `" data-ask-why="session"><span class="font-semibold">Working on:</span> Fix the build</p>`},
		{"fallback last only", AskWhy{Last: "done"},
			`<p class="` + AskWhyClass + `" data-ask-why="session"><span class="font-semibold">Last said:</span> done</p>`},
		{"agent wins", AskWhy{Text: "mine", Focus: "theirs", Last: "theirs"},
			`<p class="` + AskWhyClass + `" data-ask-why="agent"><span class="font-semibold">Why:</span> mine</p>`},
		{"none", AskWhy{}, ""},
	}
	for _, c := range cases {
		if got := AskContextLine(c.in); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
