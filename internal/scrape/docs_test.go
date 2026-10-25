package scrape

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makePDF builds a small valid PDF with one text line per page.
func makePDF(pages ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(pages)
	kids := make([]string, n)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, text := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		stream := fmt.Sprintf("BT /F1 18 Tf 72 720 Td (%s) Tj ET", text)
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

func makeDOCX(t *testing.T, documentXML string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(documentXML))
	zw.Close()
	return b.Bytes()
}

func serve(t *testing.T, contentType string, body []byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestScrapePDF(t *testing.T) {
	u := serve(t, "application/pdf", makePDF("Kochi Metro annual report", "Ridership grew 12 percent"))
	doc, err := Scrape(context.Background(), testClient(), u+"/report.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "Kochi Metro annual report") || !strings.Contains(doc.Markdown, "Ridership grew 12 percent") {
		t.Fatalf("pdf text missing:\n%s", doc.Markdown)
	}
	if doc.Metadata.NumPages != 2 || doc.Metadata.ContentType != "application/pdf" {
		t.Fatalf("metadata: %+v", doc.Metadata)
	}
}

func TestScrapePDFSniffedWithoutContentType(t *testing.T) {
	u := serve(t, "application/octet-stream", makePDF("Sniffed by magic bytes"))
	doc, err := Scrape(context.Background(), testClient(), u+"/download?id=7", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "Sniffed by magic bytes") {
		t.Fatalf("got %q", doc.Markdown)
	}
}

func TestScrapeDOCX(t *testing.T) {
	const w = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	xml := `<w:document ` + w + `><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Trip plan</w:t></w:r></w:p>
<w:p><w:r><w:t>Day one in </w:t></w:r><w:r><w:t>Munnar.</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/></w:numPr></w:pPr><w:r><w:t>Tea gardens</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Place</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Hours</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Mattupetty</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`
	u := serve(t, mimeDOCX, makeDOCX(t, xml))
	doc, err := Scrape(context.Background(), testClient(), u+"/plan.docx", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Trip plan\n\nDay one in Munnar.\n\n- Tea gardens\n\n| Place | Hours |\n| --- | --- |\n| Mattupetty | 2 |"
	if doc.Markdown != want {
		t.Fatalf("docx markdown:\n%s\n--- want ---\n%s", doc.Markdown, want)
	}
}

func TestScannedPDFReportsNoText(t *testing.T) {
	u := serve(t, "application/pdf", makePDF(""))
	_, err := Scrape(context.Background(), testClient(), u+"/scan.pdf", Options{})
	var se *Error
	if !asErr(err, &se) || se.Code != "SCRAPE_PDF_FAILED" {
		t.Fatalf("want SCRAPE_PDF_FAILED for a PDF with no text, got %v", err)
	}
}

func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
