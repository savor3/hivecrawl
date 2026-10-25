package scrape

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Documents (PDF and DOCX) are converted to markdown in-process.

const (
	mimePDF  = "application/pdf"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// docKind sniffs PDF/DOCX from the content type, URL and first bytes.
func docKind(mt, rawURL string, body []byte) string {
	lower := strings.ToLower(rawURL)
	switch {
	case mt == mimePDF || bytes.HasPrefix(body, []byte("%PDF-")):
		return "pdf"
	case mt == mimeDOCX,
		bytes.HasPrefix(body, []byte("PK\x03\x04")) && (strings.HasSuffix(lower, ".docx") || bytes.Contains(body[:min(len(body), 4096)], []byte("word/"))):
		return "docx"
	}
	return ""
}

// pdfToMarkdown extracts text page by page.
func pdfToMarkdown(body []byte) (string, int, error) {
	r, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", 0, fmt.Errorf("read pdf: %w", err)
	}
	n := r.NumPage()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := pageText(p)
		if err != nil {
			continue // one unreadable page should not lose the rest
		}
		text = tidyLines(text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	if b.Len() == 0 && n > 0 {
		return "", n, errors.New("no extractable text (the PDF may be scanned images)")
	}
	return b.String(), n, nil
}

// pageText rebuilds a page's text from glyph positions. Many PDFs store
// text as kerned fragments ("impro" "ving") with no space characters, so
// spaces are inserted where the gap between glyphs is wider than a fraction
// of the font size, and lines break when the baseline moves.
func pageText(p pdf.Page) (s string, err error) {
	defer func() {
		if r := recover(); r != nil { // the parser panics on some malformed pages
			err = fmt.Errorf("pdf page: %v", r)
		}
	}()
	glyphs := p.Content().Text
	if len(glyphs) == 0 {
		return p.GetPlainText(nil)
	}
	var b strings.Builder
	var prev *pdf.Text
	for i := range glyphs {
		g := &glyphs[i]
		if g.S == "\n" || g.S == "" {
			continue
		}
		if prev != nil {
			fs := max(prev.FontSize, g.FontSize, 1)
			dy := g.Y - prev.Y
			if dy < 0 {
				dy = -dy
			}
			gap := g.X - (prev.X + prev.W)
			switch {
			case dy > fs*0.5 || gap < -fs*2:
				b.WriteString("\n") // new baseline, or jumped back to a line start
			case gap > fs*0.15 && !strings.HasSuffix(prev.S, " ") && !strings.HasPrefix(g.S, " "):
				b.WriteString(" ")
			}
		}
		b.WriteString(g.S)
		prev = g
	}
	return b.String(), nil
}

// tidyLines collapses runs of spaces and blank lines.
func tidyLines(s string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// docxToMarkdown converts word/document.xml: headings, paragraphs, list
// items, line breaks and tables.
func docxToMarkdown(body []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("read docx: %w", err)
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", errors.New("not a Word document (no word/document.xml)")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, 50<<20))

	var (
		out       []string
		para      strings.Builder
		style     string
		isList    bool
		inTable   bool
		cell      []string
		curRow    []string
		tableRows [][]string
	)
	flushPara := func() string {
		text := strings.TrimSpace(para.String())
		para.Reset()
		if text == "" {
			return ""
		}
		switch {
		case strings.HasPrefix(style, "Heading"), style == "Title":
			level := 1
			if style != "Title" {
				fmt.Sscanf(strings.TrimPrefix(style, "Heading"), "%d", &level)
			}
			level = min(max(level, 1), 6)
			return strings.Repeat("#", level) + " " + text
		case isList:
			return "- " + text
		}
		return text
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				style, isList = "", false
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						style = a.Value
					}
				}
			case "numPr":
				isList = true
			case "tab":
				para.WriteString("\t")
			case "br":
				para.WriteString("\n")
			case "tbl":
				inTable, tableRows = true, nil
			case "tr":
				curRow = nil
			case "tc":
				cell = nil
			case "t":
				// Text lives in <w:t>; DecodeElement also consumes its end tag.
				var s string
				if err := dec.DecodeElement(&s, &t); err == nil {
					para.WriteString(s)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				text := flushPara()
				if inTable {
					if text != "" {
						cell = append(cell, text)
					}
				} else if text != "" {
					out = append(out, text)
				}
			case "tc":
				curRow = append(curRow, strings.Join(cell, " "))
			case "tr":
				tableRows = append(tableRows, curRow)
			case "tbl":
				inTable = false
				if md := markdownTable(tableRows); md != "" {
					out = append(out, md)
				}
			}
		}
	}
	return strings.Join(out, "\n\n"), nil
}

func markdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return ""
	}
	line := func(r []string) string {
		cells := make([]string, cols)
		for i := range cells {
			if i < len(r) {
				cells[i] = strings.ReplaceAll(r[i], "|", "\\|")
			}
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	var b strings.Builder
	b.WriteString(line(rows[0]) + "\n|" + strings.Repeat(" --- |", cols))
	for _, r := range rows[1:] {
		b.WriteString("\n" + line(r))
	}
	return b.String()
}
