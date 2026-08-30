package bot

import (
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"

	"github.com/nextster/telegram-bridge/internal/db"
)

func formatCodexSnapshot(snapshot db.CodexThreadSnapshot) string {
	message := strings.TrimSpace(snapshot.Message)
	if message != "" {
		return truncateTelegramHTML(markdownToTelegramHTML(truncateUTF16(message, 3600)))
	}
	switch snapshot.Status {
	case "active":
		return "В работе…"
	case "systemError":
		return "Не получилось завершить задачу."
	default:
		return "Пока без ответа."
	}
}

func markdownToTelegramHTML(markdown string) string {
	source := []byte(strings.TrimSpace(markdown))
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source))
	var out strings.Builder
	renderTelegramNode(&out, document, source)
	return strings.TrimSpace(out.String())
}

func renderTelegramNode(out *strings.Builder, node ast.Node, source []byte) {
	switch typed := node.(type) {
	case *ast.Document:
		renderTelegramChildren(out, node, source)
	case *ast.Paragraph:
		renderTelegramChildren(out, node, source)
		out.WriteString("\n\n")
	case *ast.Heading:
		out.WriteString("<b>")
		renderTelegramChildren(out, node, source)
		out.WriteString("</b>\n\n")
	case *ast.Text:
		out.WriteString(html.EscapeString(string(typed.Value(source))))
		if typed.HardLineBreak() || typed.SoftLineBreak() {
			out.WriteByte('\n')
		}
	case *ast.String:
		out.WriteString(html.EscapeString(string(typed.Value)))
	case *ast.CodeSpan:
		out.WriteString("<code>")
		out.WriteString(html.EscapeString(string(typed.Text(source))))
		out.WriteString("</code>")
	case *ast.Emphasis:
		tag := "i"
		if typed.Level >= 2 {
			tag = "b"
		}
		out.WriteString("<" + tag + ">")
		renderTelegramChildren(out, node, source)
		out.WriteString("</" + tag + ">")
	case *extensionast.Strikethrough:
		out.WriteString("<s>")
		renderTelegramChildren(out, node, source)
		out.WriteString("</s>")
	case *ast.Link:
		destination := strings.TrimSpace(string(typed.Destination))
		if parsed, err := url.Parse(destination); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			out.WriteString("<a href=\"" + html.EscapeString(destination) + "\">")
			renderTelegramChildren(out, node, source)
			out.WriteString("</a>")
		} else {
			renderTelegramChildren(out, node, source)
		}
	case *ast.AutoLink:
		destination := string(typed.URL(source))
		out.WriteString("<a href=\"" + html.EscapeString(destination) + "\">" + html.EscapeString(string(typed.Label(source))) + "</a>")
	case *ast.FencedCodeBlock:
		language := strings.TrimSpace(string(typed.Language(source)))
		out.WriteString("<pre><code")
		if language != "" {
			out.WriteString(" class=\"language-" + html.EscapeString(language) + "\"")
		}
		out.WriteString(">" + html.EscapeString(string(typed.Text(source))) + "</code></pre>\n\n")
	case *ast.CodeBlock:
		out.WriteString("<pre>" + html.EscapeString(string(typed.Text(source))) + "</pre>\n\n")
	case *ast.Blockquote:
		out.WriteString("<blockquote>")
		renderTelegramChildren(out, node, source)
		out.WriteString("</blockquote>\n")
	case *ast.List:
		renderTelegramChildren(out, node, source)
		out.WriteByte('\n')
	case *ast.ListItem:
		prefix := "• "
		if list, ok := typed.Parent().(*ast.List); ok && list.IsOrdered() {
			index := list.Start
			for sibling := typed.PreviousSibling(); sibling != nil; sibling = sibling.PreviousSibling() {
				index++
			}
			prefix = strconv.Itoa(index) + ". "
		}
		out.WriteString(prefix)
		renderTelegramChildren(out, node, source)
		out.WriteByte('\n')
	case *ast.ThematicBreak:
		out.WriteString("———\n")
	case *ast.RawHTML:
		out.WriteString(html.EscapeString(string(typed.Text(source))))
	default:
		renderTelegramChildren(out, node, source)
	}
}

func renderTelegramChildren(out *strings.Builder, node ast.Node, source []byte) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		renderTelegramNode(out, child, source)
	}
}

func truncateTelegramHTML(value string) string {
	if len([]rune(value)) <= 3900 {
		return value
	}
	// Avoid cutting through an HTML entity or tag. Long responses fall back to
	// escaped plain text while the full answer remains available in Codex.
	plain := html.EscapeString(truncateUTF16(stripTelegramHTML(value), 3800))
	return plain + "…"
}

func stripTelegramHTML(value string) string {
	var out strings.Builder
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				out.WriteRune(r)
			}
		}
	}
	return html.UnescapeString(out.String())
}
