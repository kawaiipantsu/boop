package webclient

import (
	"strings"
	"testing"
)

func TestExtractTextMetadata(t *testing.T) {
	const page = `<!DOCTYPE html>
<html lang="en">
<head>
	<title>  The   Page Title </title>
	<meta name="description" content="A &amp; short summary.">
	<link rel="stylesheet" href="/a.css">
	<link rel="canonical" href="/canonical/page">
	<meta property="og:title" content="OG title">
</head>
<body><p>Body.</p></body>
</html>`

	doc := ExtractText(page, "https://example.com/dir/index.html")
	if doc.Title != "The Page Title" {
		t.Errorf("Title = %q", doc.Title)
	}
	if doc.Description != "A & short summary." {
		t.Errorf("Description = %q", doc.Description)
	}
	if doc.Canonical != "https://example.com/canonical/page" {
		t.Errorf("Canonical = %q", doc.Canonical)
	}
	if doc.Text != "Body." {
		t.Errorf("Text = %q", doc.Text)
	}
}

func TestExtractTextFallsBackToOpenGraph(t *testing.T) {
	doc := ExtractText(`<html><head><meta property="og:title" content="Only OG">
		<meta property="og:description" content="OG desc"></head><body>x</body></html>`, "")
	if doc.Title != "Only OG" || doc.Description != "OG desc" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestExtractTextStripsNoise(t *testing.T) {
	const page = `<html><head><title>T</title><style>body{color:red}</style></head>
<body>
	<nav><a href="/nav">Navigation link</a></nav>
	<header>Site header</header>
	<script type="text/javascript">var secret = "leak";</script>
	<noscript>Enable JavaScript</noscript>
	<svg><path d="M0 0"/></svg>
	<!-- a comment with words -->
	<p>Real content.</p>
	<footer>Copyright notice</footer>
</body></html>`

	doc := ExtractText(page, "https://example.com/")
	for _, unwanted := range []string{"secret", "leak", "color:red", "Navigation link", "Site header", "Enable JavaScript", "Copyright notice", "a comment with words", "M0 0"} {
		if strings.Contains(doc.Text, unwanted) {
			t.Errorf("text still contains %q:\n%s", unwanted, doc.Text)
		}
	}
	if !strings.Contains(doc.Text, "Real content.") {
		t.Errorf("real content was lost: %q", doc.Text)
	}
	if len(doc.Links) != 0 {
		t.Errorf("links from stripped <nav> were kept: %+v", doc.Links)
	}
}

func TestExtractTextStructure(t *testing.T) {
	const page = `<body>
	<h1>Heading</h1>
	<p>First <b>bold</b> paragraph.</p>
	<p>Second paragraph.</p>
	<ul><li>Item one</li><li>Item two</li></ul>
	<div>Line A<br>Line B</div>
	</body>`

	doc := ExtractText(page, "")
	want := []string{
		"Heading",
		"First bold paragraph.",
		"Second paragraph.",
		"- Item one",
		"- Item two",
		"Line A",
		"Line B",
	}
	lines := strings.Split(doc.Text, "\n")
	var idx int
	for _, line := range lines {
		if idx < len(want) && strings.TrimSpace(line) == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("expected lines %v in order; got:\n%s", want, doc.Text)
	}
	if strings.Contains(doc.Text, "\n\n\n") {
		t.Errorf("blank runs were not collapsed:\n%q", doc.Text)
	}
}

func TestExtractTextEntities(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{"named", `<p>Tom &amp; Jerry &lt;tag&gt; &quot;quoted&quot;</p>`, `Tom & Jerry <tag> "quoted"`},
		{"nbsp collapses", `<p>a&nbsp;&nbsp;b</p>`, "a b"},
		{"numeric decimal", `<p>&#72;&#105;</p>`, "Hi"},
		{"numeric hex", `<p>&#x48;&#x69;</p>`, "Hi"},
		{"accented named", `<p>caf&eacute;</p>`, "café"},
		{"emoji", `<p>&#128512;</p>`, "\U0001F600"},
		{"unknown entity left alone", `<p>&notarealentity;</p>`, "¬arealentity;"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractText(tc.html, "").Text; got != tc.want {
				t.Fatalf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractTextLinks(t *testing.T) {
	const page = `<body>
	<a href="/relative">Relative</a>
	<a href="page.html">Sibling</a>
	<a href="https://other.example/abs">Absolute</a>
	<a href="//cdn.example/proto">Protocol relative</a>
	<a href="#fragment">Fragment</a>
	<a href="mailto:x@example.com">Mail</a>
	<a href="javascript:alert(1)">JS</a>
	<a class="x" href="/relative">Duplicate</a>
	<a>No href</a>
	<a href="/attrs-reversed" title="t">Order</a>
	</body>`

	doc := ExtractText(page, "https://example.com/dir/index.html")
	want := []Link{
		{URL: "https://example.com/relative", Text: "Relative"},
		{URL: "https://example.com/dir/page.html", Text: "Sibling"},
		{URL: "https://other.example/abs", Text: "Absolute"},
		{URL: "https://cdn.example/proto", Text: "Protocol relative"},
		{URL: "https://example.com/attrs-reversed", Text: "Order"},
	}
	if len(doc.Links) != len(want) {
		t.Fatalf("got %d links, want %d: %+v", len(doc.Links), len(want), doc.Links)
	}
	for i, w := range want {
		if doc.Links[i] != w {
			t.Errorf("link %d = %+v, want %+v", i, doc.Links[i], w)
		}
	}
}

func TestExtractTextLinksWithoutBase(t *testing.T) {
	doc := ExtractText(`<body><a href="/rel">Rel</a><a href="https://x.example/">Abs</a></body>`, "")
	if len(doc.Links) != 1 || doc.Links[0].URL != "https://x.example/" {
		t.Fatalf("without a base URL only absolute links are usable, got %+v", doc.Links)
	}
}

func TestExtractTextTruncation(t *testing.T) {
	body := "<p>" + strings.Repeat("word ", 500) + "</p>"
	doc := ExtractTextWithOptions(body, "", ExtractOptions{MaxTextRunes: 100})
	if !doc.Truncated {
		t.Fatal("truncation not reported")
	}
	if !strings.HasSuffix(doc.Text, truncationNotice) {
		t.Fatalf("truncated text must say so: %q", doc.Text[len(doc.Text)-40:])
	}
	if len([]rune(doc.Text)) != 100+len([]rune(truncationNotice)) {
		t.Fatalf("text length = %d runes", len([]rune(doc.Text)))
	}
}

func TestExtractTextLinkCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<body>")
	for i := 0; i < 20; i++ {
		sb.WriteString(`<a href="/p`)
		sb.WriteString(string(rune('a' + i)))
		sb.WriteString(`">link</a>`)
	}
	sb.WriteString("</body>")
	doc := ExtractTextWithOptions(sb.String(), "https://example.com/", ExtractOptions{MaxLinks: 5})
	if len(doc.Links) != 5 || !doc.Truncated {
		t.Fatalf("got %d links truncated=%v, want 5 and truncated", len(doc.Links), doc.Truncated)
	}
	none := ExtractTextWithOptions(sb.String(), "https://example.com/", ExtractOptions{MaxLinks: -1})
	if len(none.Links) != 0 {
		t.Fatalf("MaxLinks < 0 must collect no links, got %d", len(none.Links))
	}
}

func TestExtractTextMalformedHTML(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		contains string
		absent   string
	}{
		{"unclosed p tags", "<p>one<p>two<p>three", "two", ""},
		{"unclosed script drops rest", "<body>visible<script>hidden()", "visible", "hidden()"},
		{"stray angle bracket", "<p>5 < 7 is true</p>", "5", ""},
		{"attribute with angle bracket entity", `<p title="a &gt; b">text</p>`, "text", ""},
		{"nested identical tags", "<div><div><p>deep</p></div></div>", "deep", ""},
		{"uppercase tags", "<BODY><P>Upper</P></BODY>", "Upper", ""},
		{"tag soup", "<b><i>bold italic</b></i>", "bold italic", ""},
		{"empty document", "", "", ""},
		{"plain text only", "no markup at all", "no markup at all", ""},
		{"cdata removed", "<body>keep<![CDATA[drop me]]></body>", "keep", "drop me"},
		{"comment inside script", "<body>ok<script><!-- x --></script></body>", "ok", "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := ExtractText(tc.html, "https://example.com/")
			if tc.contains != "" && !strings.Contains(doc.Text, tc.contains) {
				t.Errorf("text %q does not contain %q", doc.Text, tc.contains)
			}
			if tc.absent != "" && strings.Contains(doc.Text, tc.absent) {
				t.Errorf("text %q must not contain %q", doc.Text, tc.absent)
			}
		})
	}
}

func TestExtractTextInlineTagsDoNotFuseWords(t *testing.T) {
	doc := ExtractText("<p>one<span>two</span>three</p>", "")
	if strings.Contains(doc.Text, "onetwothree") {
		t.Fatalf("inline tags fused words: %q", doc.Text)
	}
}

func TestParseAttrs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"double quoted", `href="/a" class="b"`, map[string]string{"href": "/a", "class": "b"}},
		{"reversed order", `class="b" href="/a"`, map[string]string{"href": "/a", "class": "b"}},
		{"single quoted", `href='/a'`, map[string]string{"href": "/a"}},
		{"unquoted", `href=/a`, map[string]string{"href": "/a"}},
		{"uppercase name", `HREF="/a"`, map[string]string{"href": "/a"}},
		{"valueless", `disabled href="/a"`, map[string]string{"disabled": "", "href": "/a"}},
		{"entity in value", `href="/a?x=1&amp;y=2"`, map[string]string{"href": "/a?x=1&y=2"}},
		{"extra whitespace", `  href = "/a"  `, map[string]string{"href": "/a"}},
		{"data attributes", `data-id="5" href="/a"`, map[string]string{"data-id": "5", "href": "/a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAttrs(tc.in)
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("attr %q = %q, want %q (all: %v)", k, got[k], want, got)
				}
			}
		})
	}
}

func TestHasClass(t *testing.T) {
	tests := []struct {
		class string
		name  string
		want  bool
	}{
		{"result-link", "result-link", true},
		{"a result-link b", "result-link", true},
		{"result-links", "result-link", false},
		{"RESULT-LINK", "result-link", true},
		{"", "result-link", false},
	}
	for _, tc := range tests {
		if got := hasClass(map[string]string{"class": tc.class}, tc.name); got != tc.want {
			t.Errorf("hasClass(%q, %q) = %v, want %v", tc.class, tc.name, got, tc.want)
		}
	}
}

func TestSchemeOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x/y", "https"},
		{"/path:with:colon", ""},
		{"mailto:a@b", "mailto"},
		{"relative.html", ""},
		{"//cdn.example/x", ""},
		{"?q=a:b", ""},
	}
	for _, tc := range tests {
		if got := schemeOf(tc.in); got != tc.want {
			t.Errorf("schemeOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
