package documents

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Tiny PDFs written here, so the tests need no fixture files.

type fixturePage struct {
	content string // the page's content stream
	form    string // content of Form XObject /Fm1, if any
}

type pdfOptions struct {
	compress bool   // FlateDecode the content streams
	locked   bool   // encrypted with a user password
	header   string // "" = %PDF-1.4
	trailing string // bytes after %%EOF
}

func pdfString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return "(" + r.Replace(s) + ")"
}

// textContent draws s as real text (Helvetica, 18 pt).
func textContent(s string) string {
	if s == "" {
		return ""
	}
	return "BT /F1 18 Tf 72 700 Td " + pdfString(s) + " Tj ET"
}

// scanContent is a page that is only a picture.
const scanContent = "q 612 0 0 792 0 0 cm /Im1 Do Q"

func textPDF(pages ...string) []byte {
	var ps []fixturePage
	for _, p := range pages {
		ps = append(ps, fixturePage{content: textContent(p)})
	}
	return buildPDF(ps, pdfOptions{})
}

func scannedPDF(n int) []byte {
	var ps []fixturePage
	for range n {
		ps = append(ps, fixturePage{content: scanContent})
	}
	return buildPDF(ps, pdfOptions{})
}

func mixedPDF(text string) []byte {
	return buildPDF([]fixturePage{{content: textContent(text)}, {content: scanContent}}, pdfOptions{})
}

func buildPDF(pages []fixturePage, o pdfOptions) []byte {
	var objs []string // objs[i] is object i+1
	add := func(s string) int { objs = append(objs, s); return len(objs) }
	stream := func(dict, data string) string {
		if o.compress {
			var b bytes.Buffer
			z := zlib.NewWriter(&b)
			io.WriteString(z, data)
			z.Close()
			data = b.String()
			dict += " /Filter /FlateDecode"
		}
		return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
	}

	catalog := add("") // filled in below
	pagesObj := add("")
	f1 := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	img := add("<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 4 >>\nstream\n\x00\xff\xff\x00\nendstream")
	// F2: a Type0 font whose two-byte codes 1-5 map to "Hello" through ToUnicode.
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /T def /CMapType 2 def\n" +
		"1 begincodespacerange <0000> <FFFF> endcodespacerange\n" +
		"2 beginbfchar <0001> <0048> <0002> <0065> endbfchar\n" +
		"1 beginbfrange <0003> <0004> <006C> endbfrange\n" +
		"1 beginbfchar <0005> <006F> endbfchar\n" +
		"endcmap CMapName currentdict /CMap defineresource pop end end"
	toUnicode := add(stream("", cmap))
	f2 := add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /ABCDEF+Noto /Encoding /Identity-H /ToUnicode %d 0 R >>", toUnicode))

	var kids []string
	for _, p := range pages {
		xobjects := fmt.Sprintf("/Im1 %d 0 R", img)
		if p.form != "" {
			form := add(stream("/Type /XObject /Subtype /Form /BBox [0 0 612 792]", p.form))
			xobjects += fmt.Sprintf(" /Fm1 %d 0 R", form)
		}
		content := add(stream("", p.content))
		page := add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R /F2 %d 0 R >> /XObject << %s >> >> /Contents %d 0 R >>",
			pagesObj, f1, f2, xobjects, content))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objs[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObj)
	objs[pagesObj-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))
	trailerExtra := ""
	if o.locked {
		// A user password: no empty-password key matches U.
		enc := add("<< /Filter /Standard /V 1 /R 2 /O <" + strings.Repeat("ab", 32) + "> /U <" + strings.Repeat("cd", 32) + "> /P -4 >>")
		trailerExtra = fmt.Sprintf(" /Encrypt %d 0 R /ID [<0123456789abcdef0123456789abcdef> <0123456789abcdef0123456789abcdef>]", enc)
	}

	header := o.header
	if header == "" {
		header = "%PDF-1.4"
	}
	var b bytes.Buffer
	b.WriteString(header + "\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objs))
	for i, s := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, s)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R%s >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, catalog, trailerExtra, xref)
	b.WriteString(o.trailing)
	return b.Bytes()
}

// fakeOCR returns canned text (or err) and counts calls.
type fakeOCR struct {
	mu    sync.Mutex
	text  string
	err   error
	calls int
}

func (f *fakeOCR) PDFPage(context.Context, []byte, int) (string, error) { return f.answer() }
func (f *fakeOCR) Image(context.Context, []byte, string) (string, error) {
	return f.answer()
}

func (f *fakeOCR) answer() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.text, f.err
}

func (f *fakeOCR) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// stubTransport answers every request from a handler and records them.
type stubTransport struct {
	mu      sync.Mutex
	seen    []seenRequest
	handler func(r seenRequest) (int, string, error)
}

type seenRequest struct {
	method, url string
	body        []byte
}

func stub(h func(r seenRequest) (int, string, error)) *stubTransport {
	return &stubTransport{handler: h}
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}
	seen := seenRequest{r.Method, r.URL.String(), body}
	s.mu.Lock()
	s.seen = append(s.seen, seen)
	s.mu.Unlock()
	status, text, err := s.handler(seen)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(text)), Header: http.Header{}, Request: r}, nil
}

func (s *stubTransport) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

func (s *stubTransport) count(method string) int {
	n := 0
	for _, r := range s.requests() {
		if r.method == method {
			n++
		}
	}
	return n
}

func (s *stubTransport) client() *http.Client { return &http.Client{Transport: s} }
