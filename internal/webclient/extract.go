package webclient

import (
	stdhtml "html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Extraction defaults.
const (
	// DefaultMaxTextRunes caps extracted text. Roughly 25k tokens: enough
	// for a long article, small enough not to swallow a context window.
	DefaultMaxTextRunes = 100_000
	// DefaultMaxLinks caps how many links are reported.
	DefaultMaxLinks = 200
	// truncationNotice is appended to text that was cut short, so the model
	// knows it is not looking at the whole page.
	truncationNotice = "\n\n[truncated]"
)

// Link is one resolved hyperlink from a page.
type Link struct {
	// URL is absolute, resolved against the page's base URL.
	URL string
	// Text is the link's visible text, whitespace-collapsed.
	Text string
}

// Document is the readable content extracted from an HTML page.
type Document struct {
	// Title is the <title> text, or the og:title if there is no <title>.
	Title string
	// Description is the meta description, or og:description.
	Description string
	// Canonical is the rel=canonical URL, resolved absolute.
	Canonical string
	// Text is the readable body text.
	Text string
	// Links are the resolved hyperlinks found in the readable content.
	Links []Link
	// Truncated reports that Text or Links hit a cap.
	Truncated bool
}

// ExtractOptions bounds an extraction.
type ExtractOptions struct {
	// MaxTextRunes caps the text length. Zero selects DefaultMaxTextRunes.
	MaxTextRunes int
	// MaxLinks caps the link list. Zero selects DefaultMaxLinks. A negative
	// value collects no links.
	MaxLinks int
}

// Tags whose contents are never readable page text.
var dropTags = []string{"script", "style", "noscript", "svg", "head", "nav", "header", "footer", "template", "iframe", "canvas", "math", "object", "audio", "video", "form", "select"}

// Tags that imply a line break when they open or close.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"body": true, "br": true, "caption": true, "dd": true, "div": true,
	"dl": true, "dt": true, "fieldset": true, "figcaption": true,
	"figure": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "hr": true, "main": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tbody": true,
	"td": true, "tfoot": true, "th": true, "thead": true, "tr": true,
	"ul": true,
}

var (
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	doctypeRe = regexp.MustCompile(`(?is)<!doctype[^>]*>`)
	cdataRe   = regexp.MustCompile(`(?s)<!\[CDATA\[.*?\]\]>`)
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaRe    = regexp.MustCompile(`(?is)<meta\s([^>]*)>`)
	linkTagRe = regexp.MustCompile(`(?is)<link\s([^>]*)>`)
	anchorRe  = regexp.MustCompile(`(?is)<a\s([^>]*)>(.*?)</a>`)
	tagRe     = regexp.MustCompile(`(?s)</?([a-zA-Z][a-zA-Z0-9:-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>`)
	spacesRe  = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankRe   = regexp.MustCompile(`\n{3,}`)
	attrRe    = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	bodyRe    = regexp.MustCompile(`(?is)<body[^>]*>`)
)

// dropOpenRe holds one precompiled opening-tag matcher per dropped element.
var dropOpenRe = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(dropTags))
	for _, tag := range dropTags {
		m[tag] = regexp.MustCompile(`(?is)<` + tag + `(\s[^>]*)?>`)
	}
	return m
}()

// ExtractText turns an HTML document into readable plain text and metadata,
// with links resolved against baseURL.
//
// This is a tolerant, regexp-driven extractor rather than a real HTML parser:
// Boop deliberately carries no HTML-parsing dependency. It is built for the
// common case of getting a page into a model's context, and it favours clean
// readable output over structural fidelity. See the package tests for the
// shapes it handles; see the doc comment on Document for what it will not do.
//
// Known limits: it does not build a tree, so it cannot tell a <div> inside an
// article from one inside a widget; unclosed drop-tags (an unterminated
// <script>, say) only have their opening tag removed; and attribute values
// containing an unescaped ">" can confuse tag boundaries. Malformed input
// degrades into extra or missing whitespace, never a panic.
func ExtractText(htmlSrc, baseURL string) Document {
	return ExtractTextWithOptions(htmlSrc, baseURL, ExtractOptions{})
}

// ExtractTextWithOptions is ExtractText with explicit caps.
func ExtractTextWithOptions(htmlSrc, baseURL string, opts ExtractOptions) Document {
	if opts.MaxTextRunes == 0 {
		opts.MaxTextRunes = DefaultMaxTextRunes
	}
	if opts.MaxLinks == 0 {
		opts.MaxLinks = DefaultMaxLinks
	}
	var base *url.URL
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil && u.IsAbs() {
			base = u
		}
	}

	doc := Document{}
	src := cdataRe.ReplaceAllString(htmlSrc, " ")
	src = commentRe.ReplaceAllString(src, " ")
	src = doctypeRe.ReplaceAllString(src, " ")

	// Metadata comes from the head, so read it before the head is dropped.
	doc.Title, doc.Description, doc.Canonical = extractMetadata(src, base)

	body := src
	if i := regexpBodyStart(src); i >= 0 {
		body = src[i:]
	}
	for _, tag := range dropTags {
		body = dropElement(body, tag)
	}

	if opts.MaxLinks > 0 {
		doc.Links = extractLinks(body, base, opts.MaxLinks)
		if len(doc.Links) == opts.MaxLinks {
			doc.Truncated = true
		}
	}

	text := tagsToText(body)
	text = stdhtml.UnescapeString(text)
	text = normalizeWhitespace(text)
	if n := utf8.RuneCountInString(text); n > opts.MaxTextRunes {
		text = string([]rune(text)[:opts.MaxTextRunes]) + truncationNotice
		doc.Truncated = true
	}
	doc.Text = text
	return doc
}

// regexpBodyStart returns the offset just after <body ...>, or -1.
func regexpBodyStart(s string) int {
	loc := bodyRe.FindStringIndex(s)
	if loc == nil {
		return -1
	}
	return loc[1]
}

// extractMetadata pulls the title, description and canonical URL out of the
// raw document.
func extractMetadata(src string, base *url.URL) (title, desc, canonical string) {
	if m := titleRe.FindStringSubmatch(src); m != nil {
		title = collapseInline(stdhtml.UnescapeString(stripTags(m[1])))
	}
	var ogTitle string
	for _, m := range metaRe.FindAllStringSubmatch(src, -1) {
		attrs := parseAttrs(m[1])
		name := strings.ToLower(attrs["name"])
		prop := strings.ToLower(attrs["property"])
		content := collapseInline(stdhtml.UnescapeString(attrs["content"]))
		if content == "" {
			continue
		}
		switch {
		case desc == "" && (name == "description" || prop == "og:description"):
			desc = content
		case ogTitle == "" && prop == "og:title":
			ogTitle = content
		}
	}
	if title == "" {
		title = ogTitle
	}
	for _, m := range linkTagRe.FindAllStringSubmatch(src, -1) {
		attrs := parseAttrs(m[1])
		if strings.EqualFold(strings.TrimSpace(attrs["rel"]), "canonical") {
			canonical = resolveURL(base, attrs["href"])
			break
		}
	}
	return title, desc, canonical
}

// extractLinks collects hyperlinks with absolute URLs.
func extractLinks(body string, base *url.URL, max int) []Link {
	var links []Link
	seen := make(map[string]bool)
	for _, m := range anchorRe.FindAllStringSubmatch(body, -1) {
		attrs := parseAttrs(m[1])
		href := strings.TrimSpace(attrs["href"])
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		switch strings.ToLower(schemeOf(href)) {
		case "", "http", "https":
		default:
			// javascript:, mailto:, data: and friends are not fetchable.
			continue
		}
		abs := resolveURL(base, href)
		if abs == "" || seen[abs] {
			continue
		}
		seen[abs] = true
		links = append(links, Link{
			URL:  abs,
			Text: collapseInline(stdhtml.UnescapeString(stripTags(m[2]))),
		})
		if len(links) >= max {
			break
		}
	}
	return links
}

// schemeOf returns the URL scheme of href, or "" when it is relative.
func schemeOf(href string) string {
	i := strings.IndexByte(href, ':')
	if i <= 0 {
		return ""
	}
	if j := strings.IndexAny(href, "/?#"); j >= 0 && j < i {
		return ""
	}
	return href[:i]
}

// resolveURL makes href absolute against base. When base is unknown, an
// already-absolute href is kept and a relative one is dropped: a bare "/docs"
// is useless to a caller that cannot tell which site it came from.
func resolveURL(base *url.URL, href string) string {
	h := strings.TrimSpace(href)
	if h == "" {
		return ""
	}
	u, err := url.Parse(h)
	if err != nil {
		return ""
	}
	if base == nil {
		if u.IsAbs() {
			return u.String()
		}
		return ""
	}
	return base.ResolveReference(u).String()
}

// rawTextTags hold content that is never rendered as text. An unterminated one
// swallows the rest of the document, which is what a real HTML parser does and
// what stops an unclosed <script> from dumping JavaScript into the output.
var rawTextTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "iframe": true, "svg": true}

// dropElement removes <tag>…</tag> including its contents. When the closing tag
// is missing, a raw-text element takes the remainder of the document with it
// while a layout element such as an unterminated <nav> costs only its own tag.
func dropElement(s, tag string) string {
	open, ok := dropOpenRe[tag]
	if !ok {
		open = regexp.MustCompile(`(?is)<` + tag + `(\s[^>]*)?>`)
	}
	closeTag := "</" + tag
	var b strings.Builder
	for {
		loc := open.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:loc[0]])
		b.WriteString(" ")
		rest := s[loc[1]:]
		idx := indexFold(rest, closeTag)
		if idx < 0 {
			if rawTextTags[tag] {
				return b.String()
			}
			s = rest
			continue
		}
		end := strings.IndexByte(rest[idx:], '>')
		if end < 0 {
			s = rest[idx:]
			continue
		}
		s = rest[idx+end+1:]
	}
}

// indexFold is a case-insensitive strings.Index.
func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

// tagsToText replaces markup with text, inserting breaks where block elements
// and list items were so the result still reads as prose.
func tagsToText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, loc := range tagRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		last = loc[1]
		name := strings.ToLower(s[loc[2]:loc[3]])
		closing := s[loc[0]+1] == '/'
		switch {
		case name == "li" && !closing:
			b.WriteString("\n- ")
		case name == "br":
			b.WriteString("\n")
		case name == "p" && closing, isHeading(name) && closing:
			b.WriteString("\n\n")
		case blockTags[name]:
			b.WriteString("\n")
		default:
			// Inline element: keep words from fusing across the tag.
			b.WriteString(" ")
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

func isHeading(name string) bool {
	return len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6'
}

// stripTags removes markup without adding structure. Used for short fragments
// such as a link's own text.
func stripTags(s string) string {
	return tagRe.ReplaceAllString(s, " ")
}

// normalizeWhitespace collapses runs of spaces, trims each line and limits
// blank runs to a single blank line, keeping paragraph and list structure.
func normalizeWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = spacesRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	out := blankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(out)
}

// collapseInline reduces a fragment to a single whitespace-collapsed line.
func collapseInline(s string) string {
	return strings.TrimSpace(spacesRe.ReplaceAllString(strings.Join(strings.Fields(s), " "), " "))
}

// parseAttrs parses an HTML attribute list into a lowercased-key map.
//
// Attribute order is never assumed. Real markup puts them in whatever order it
// likes, and a selector that expects class before href silently returns nothing
// the day a site swaps them.
func parseAttrs(s string) map[string]string {
	attrs := make(map[string]string, 4)
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		key := strings.ToLower(m[1])
		if _, dup := attrs[key]; dup {
			continue
		}
		val := m[2]
		if val == "" {
			val = m[3]
		}
		if val == "" {
			val = m[4]
		}
		attrs[key] = stdhtml.UnescapeString(val)
	}
	return attrs
}

// hasClass reports whether an element's class attribute contains name.
func hasClass(attrs map[string]string, name string) bool {
	for _, f := range strings.Fields(attrs["class"]) {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}
