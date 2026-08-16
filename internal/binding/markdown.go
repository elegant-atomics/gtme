package binding

// A deliberately small HTML→markdown converter for http/enrich's markdown
// mode (SPEC §10a, ADR-024). It preserves reading structure — headings,
// paragraphs, lists, links, emphasis — and drops everything else. The goal is
// AI-judgeable text with provenance, not typographic fidelity; anything
// needing a real renderer is JS-heavy-page territory and routes to a
// reader-provider binding (ROADMAP).

import (
	"strings"

	"golang.org/x/net/html"
)

// HTMLToMarkdown converts an HTML document to markdown text.
func HTMLToMarkdown(raw string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(raw))
	var b strings.Builder
	var skipDepth int // inside script/style/head/noscript
	var href string
	var linkText strings.Builder
	inLink := false

	writeBlockBreak := func() {
		out := strings.TrimRight(b.String(), " \n")
		b.Reset()
		b.WriteString(out)
		if out != "" {
			b.WriteString("\n\n")
		}
	}

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := tokenizer.Token()
			name := tok.Data
			switch name {
			case "script", "style", "head", "noscript", "svg", "iframe":
				if tt == html.StartTagToken {
					skipDepth++
				}
			case "h1", "h2", "h3", "h4", "h5", "h6":
				writeBlockBreak()
				b.WriteString(strings.Repeat("#", int(name[1]-'0')) + " ")
			case "p", "div", "section", "article", "tr", "table":
				writeBlockBreak()
			case "br":
				b.WriteString("\n")
			case "li":
				writeBlockBreak()
				b.WriteString("- ")
			case "a":
				inLink = true
				href = attr(tok, "href")
				linkText.Reset()
			case "strong", "b":
				b.WriteString("**")
			case "em", "i":
				b.WriteString("*")
			}
		case html.EndTagToken:
			name := tokenizer.Token().Data
			switch name {
			case "script", "style", "head", "noscript", "svg", "iframe":
				if skipDepth > 0 {
					skipDepth--
				}
			case "a":
				inLink = false
				text := collapse(linkText.String())
				switch {
				case text == "":
				case href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:"):
					b.WriteString(text)
				default:
					b.WriteString("[" + text + "](" + href + ")")
				}
			case "strong", "b":
				b.WriteString("**")
			case "em", "i":
				b.WriteString("*")
			case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "li", "section", "article":
				writeBlockBreak()
			}
		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			text := collapse(string(tokenizer.Text()))
			if text == "" {
				continue
			}
			if inLink {
				if linkText.Len() > 0 {
					linkText.WriteString(" ")
				}
				linkText.WriteString(text)
				continue
			}
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") && !strings.HasSuffix(b.String(), " ") &&
				!strings.HasSuffix(b.String(), "*") && !strings.HasSuffix(b.String(), "# ") && !strings.HasSuffix(b.String(), "- ") {
				b.WriteString(" ")
			}
			b.WriteString(text)
		}
	}
	return strings.TrimSpace(b.String())
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func attr(tok html.Token, name string) string {
	for _, a := range tok.Attr {
		if a.Key == name {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}
