package documents

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

const paddleURL = "http://ocr.test:8002/layout-parsing"

var ctx = context.Background()

func input(data []byte, mediaType string) hooks.DocumentInput {
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	return hooks.DocumentInput{Data: data, MediaType: mediaType, Title: "doc"}
}

type svcOpts struct {
	local  config.LocalExtraction
	paddle *stubTransport // non-nil = PaddleOCR configured at paddleURL
	ocr    OCR
	cap    int
}

func service(o svcOpts) *Service {
	if o.local == "" {
		o.local = config.LocalOCR
	}
	cfg := config.Documents{Local: o.local}
	s := New(cfg, "", nil)
	if o.paddle != nil {
		s.paddle = newPaddle(paddleURL, "", o.paddle.client())
	}
	if o.ocr == nil {
		o.ocr = &fakeOCR{err: errors.New("fake OCR failure")}
	}
	s.ocr = o.ocr
	if o.cap > 0 {
		s.textCap = o.cap
	}
	return s
}

const paddleOK = `{"logId":"x","errorCode":0,"errorMsg":"Success","result":{"layoutParsingResults":[{"markdown":{"text":"# Page one\n| a | b |"}},{"markdown":{"text":"Page two text"}}]}}`

// paddleServer answers POSTs with reply and the probe GET with 405.
func paddleServer(status int, reply string) *stubTransport {
	return stub(func(r seenRequest) (int, string, error) {
		if r.method == "POST" {
			return status, reply, nil
		}
		return 405, "", nil
	})
}

func down() *stubTransport {
	return stub(func(seenRequest) (int, string, error) { return 0, "", errors.New("connection refused") })
}

// MARK: PDF text layer

func TestPDFReadsTextLayerPerPage(t *testing.T) {
	ocr := &fakeOCR{err: errors.New("fail")}
	out := service(svcOpts{ocr: ocr}).Read(ctx, input(textPDF("Quarterly revenue grew twelve percent", "Second page about margins"), ""))
	if !strings.Contains(out.Text, "Quarterly revenue grew twelve percent") || !strings.Contains(out.Text, "Second page about margins") {
		t.Fatalf("%q (%s)", out.Text, out.Error)
	}
	if strings.Index(out.Text, "Quarterly") > strings.Index(out.Text, "Second page") {
		t.Fatal("page order")
	}
	if out.Via != "pdf" || out.Pages != 2 || out.OCRPages != 0 || out.Error != "" {
		t.Fatalf("%+v", out)
	}
	if ocr.count() != 0 {
		t.Fatal("OCR called")
	}
}

func TestPagesWithoutTextAreOCRd(t *testing.T) {
	ocr := &fakeOCR{text: "Scanned page words"}
	out := service(svcOpts{ocr: ocr}).Read(ctx, input(mixedPDF("This page has a real text layer"), ""))
	if !strings.Contains(out.Text, "This page has a real text layer") || !strings.Contains(out.Text, "Scanned page words") {
		t.Fatalf("%q", out.Text)
	}
	if out.Pages != 2 || out.OCRPages != 1 || out.Via != "pdf+ocr" {
		t.Fatalf("%+v", out)
	}
	if ocr.count() != 1 { // only the text-less page
		t.Fatalf("%d OCR calls", ocr.count())
	}
}

func TestOCRReadsAScannedPDF(t *testing.T) {
	out := service(svcOpts{ocr: &fakeOCR{text: "INVOICE 4721"}}).Read(ctx, input(scannedPDF(1), ""))
	if out.Text != "INVOICE 4721" || out.Via != "ocr" || out.OCRPages != 1 || out.Pages != 1 {
		t.Fatalf("%+v", out)
	}
}

// The Go build has no platform OCR yet: a scan gets the note, and OCR is
// asked once, not for every page.
func TestScannedPDFWithoutPlatformOCRGetsTheNote(t *testing.T) {
	ocr := &countingUnavailable{}
	out := service(svcOpts{ocr: ocr}).Read(ctx, input(scannedPDF(3), ""))
	if out.Text != "" || !strings.Contains(out.Error, "no text layer") || !strings.Contains(out.Error, "PaddleOCR") {
		t.Fatalf("%+v", out)
	}
	if ocr.calls.Load() != 1 {
		t.Fatalf("%d OCR calls", ocr.calls.Load())
	}
	// the real platform default
	if out := New(config.Documents{Local: config.LocalOCR}, "", nil).Read(ctx, input(scannedPDF(1), "")); !strings.Contains(out.Error, "no text layer") {
		t.Fatalf("%+v", out)
	}
}

type countingUnavailable struct{ calls atomic.Int32 }

func (c *countingUnavailable) PDFPage(context.Context, []byte, int) (string, error) {
	c.calls.Add(1)
	return "", ErrOCRUnavailable
}

func (c *countingUnavailable) Image(context.Context, []byte, string) (string, error) {
	c.calls.Add(1)
	return "", ErrOCRUnavailable
}

func TestOCRReadsAnImageSentAsADocument(t *testing.T) {
	out := service(svcOpts{ocr: &fakeOCR{text: "  HELLO WORLD \n"}}).Read(ctx, input([]byte{0x89, 'P', 'N', 'G'}, "image/png"))
	if out.Text != "HELLO WORLD" || out.Via != "ocr" || out.Pages != 1 || out.OCRPages != 1 {
		t.Fatalf("%+v", out)
	}
	none := New(config.Documents{Local: config.LocalOCR}, "", nil).Read(ctx, input([]byte{1}, "image/png"))
	if none.Text != "" || !strings.Contains(none.Error, "images need OCR") {
		t.Fatalf("%+v", none)
	}
	empty := service(svcOpts{ocr: &fakeOCR{text: " "}}).Read(ctx, input([]byte{2}, "image/png"))
	if empty.Error != "no text found in the image" {
		t.Fatalf("%+v", empty)
	}
}

func TestTextOnlyDoesNotOCR(t *testing.T) {
	ocr := &fakeOCR{text: "never"}
	out := service(svcOpts{local: config.LocalTextOnly, ocr: ocr}).Read(ctx, input(scannedPDF(1), ""))
	if out.Text != "" || !strings.Contains(out.Error, "no text layer") || ocr.count() != 0 {
		t.Fatalf("%+v, %d calls", out, ocr.count())
	}
	img := service(svcOpts{local: config.LocalTextOnly, ocr: ocr}).Read(ctx, input([]byte{1, 2}, "image/png"))
	if img.Text != "" || img.Error == "" || ocr.count() != 0 {
		t.Fatalf("%+v", img)
	}
}

func TestOffReturnsANote(t *testing.T) {
	out := service(svcOpts{local: config.LocalOff}).Read(ctx, input(textPDF("Some text that exists"), ""))
	if out.Text != "" || !strings.Contains(out.Error, "turned off") || out.Via != "off" {
		t.Fatalf("%+v", out)
	}
}

func TestUnreadableInputsReportWhy(t *testing.T) {
	s := service(svcOpts{})
	if bad := s.Read(ctx, input([]byte("not a pdf"), "")); bad.Text != "" || bad.Error != "not a readable PDF" {
		t.Fatalf("%+v", bad)
	}
	if other := s.Read(ctx, input([]byte{1, 2}, "application/zip")); other.Text != "" || !strings.Contains(other.Error, "unsupported") {
		t.Fatalf("%+v", other)
	}
	if plain := s.Read(ctx, input([]byte("hello text"), "text/plain")); plain.Text != "hello text" || plain.Via != "text" {
		t.Fatalf("%+v", plain)
	}
	if bin := s.Read(ctx, input([]byte{0xff, 0xfe}, "text/plain")); bin.Error != "not valid UTF-8 text" {
		t.Fatalf("%+v", bin)
	}
	locked := s.Read(ctx, input(buildPDF([]fixturePage{{content: textContent("secret text here and more")}}, pdfOptions{locked: true}), ""))
	if locked.Error != "the PDF is password protected" {
		t.Fatalf("%+v", locked)
	}
	// truncated mid-file
	pdf := textPDF("A page that gets cut off halfway")
	if cut := s.Read(ctx, input(pdf[:len(pdf)/2], "")); cut.Error != "not a readable PDF" {
		t.Fatalf("%+v", cut)
	}
}

// MARK: PDF layout

func pageText(t *testing.T, content string, o pdfOptions) string {
	t.Helper()
	doc, err := openPDF(buildPDF([]fixturePage{{content: content}}, o))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(doc.pageText(0))
}

func TestPDFLayoutKeepsWordsAndLines(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"TJ gap is a space", "BT /F1 12 Tf 72 700 Td [(Hello) -300 (World)] TJ ET", "Hello World"},
		{"kerning is not", "BT /F1 12 Tf 72 700 Td [(Hel) -20 (lo)] TJ ET", "Hello"},
		{"Td down is a new line", "BT /F1 12 Tf 72 700 Td (First line) Tj 0 -14 Td (Second line) Tj ET", "First line\nSecond line"},
		{"T* and TL", "BT /F1 12 Tf 14 TL 72 700 Td (One) Tj T* (Two) Tj (Three) ' ET", "One\nTwo\nThree"},
		{"text objects on one line", "BT /F1 12 Tf 72 700 Td (Left) Tj ET BT /F1 12 Tf 200 700 Td (Right) Tj ET", "Left Right"},
		{"Tm positions", "BT /F1 12 Tf 1 0 0 1 72 700 Tm (Top) Tj 1 0 0 1 72 680 Tm (Bottom) Tj ET", "Top\nBottom"},
		{"runs that touch join", "BT /F1 12 Tf 72 700 Td (Bold) Tj (face) Tj ET", "Boldface"},
		{"escapes", `BT /F1 12 Tf 72 700 Td (a \(b\) c\\d) Tj ET`, `a (b) c\d`},
		{"Type0 with ToUnicode", "BT /F2 12 Tf 72 700 Td <0001000200030003 0005 0004> Tj ET", "Hellom"},
		{"form xobject", "/Fm1 Do", "Inside a form"},
	}
	for _, c := range cases {
		o := pdfOptions{}
		content := c.content
		if c.name == "form xobject" {
			doc, err := openPDF(buildPDF([]fixturePage{{content: content, form: textContent("Inside a form")}}, o))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(doc.pageText(0)); got != c.want {
				t.Errorf("%s: %q", c.name, got)
			}
			continue
		}
		if got := pageText(t, content, o); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPDFFramingVariants(t *testing.T) {
	for name, o := range map[string]pdfOptions{
		"flate":         {compress: true},
		"pdf 2.0":       {header: "%PDF-2.0"},
		"header junk":   {header: "%PDF-1.7 generated"},
		"trailing junk": {trailing: strings.Repeat("\x00", 300) + "garbage"},
	} {
		if got := pageText(t, textContent("Framing variant text"), o); got != "Framing variant text" {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestBrokenPageStillReadsTheOthers(t *testing.T) {
	pdf := buildPDF([]fixturePage{{content: textContent("A good first page of text")}, {content: "BT /F1 12 Tf ( unterminated"}, {content: textContent("A good third page of text")}}, pdfOptions{})
	out := service(svcOpts{local: config.LocalTextOnly}).Read(ctx, input(pdf, ""))
	if !strings.Contains(out.Text, "first page") || !strings.Contains(out.Text, "third page") || out.Pages != 3 {
		t.Fatalf("%+v", out)
	}
}

// MARK: PaddleOCR

func TestPaddleOCRIsUsedFirstAndSendsTheDocument(t *testing.T) {
	st := paddleServer(200, paddleOK)
	ocr := &fakeOCR{err: errors.New("fail")}
	pdf := textPDF("local text that should not be used")
	out := service(svcOpts{paddle: st, ocr: ocr}).Read(ctx, input(pdf, ""))
	if out.Text != "# Page one\n| a | b |\n\nPage two text" || out.Via != "paddleocr" || out.Pages != 2 {
		t.Fatalf("%+v", out)
	}
	var post *seenRequest
	for _, r := range st.requests() {
		if r.method == "POST" {
			post = &r
		}
	}
	if post == nil || post.url != paddleURL {
		t.Fatal("no POST")
	}
	b := jsonx.ParseObject(post.body)
	if jsonx.String(b["fileType"]) != "0" || b["file"] != base64.StdEncoding.EncodeToString(pdf) || b["returnMarkdownImages"] != false || b["visualize"] != false {
		t.Fatalf("body %s", jsonx.String(b))
	}
	if ocr.count() != 0 {
		t.Fatal("OCR called")
	}
}

func TestPaddleOCRGetsImagesAsFileTypeOne(t *testing.T) {
	st := paddleServer(200, paddleOK)
	service(svcOpts{paddle: st}).Read(ctx, input([]byte{1, 2, 3}, "image/png"))
	for _, r := range st.requests() {
		if r.method == "POST" {
			if ft := jsonx.String(jsonx.ParseObject(r.body)["fileType"]); ft != "1" {
				t.Fatal(ft)
			}
			return
		}
	}
	t.Fatal("no POST")
}

func TestUnreachablePaddleFallsBackToLocal(t *testing.T) {
	st := down()
	out := service(svcOpts{paddle: st}).Read(ctx, input(textPDF("Local fallback text works fine"), ""))
	if !strings.Contains(out.Text, "Local fallback text works fine") || out.Via != "pdf" {
		t.Fatalf("%+v", out)
	}
	if st.count("POST") != 0 { // probe only, no document sent
		t.Fatal("document sent")
	}
}

func TestPaddleErrorFallsBackToLocal(t *testing.T) {
	replies := []struct {
		status int
		body   string
	}{{500, "boom"}, {200, `{"errorCode":10,"errorMsg":"bad"}`}, {200, "<html>"}, {200, `{"errorCode":0,"result":{"layoutParsingResults":[]}}`}}
	for _, r := range replies {
		st := paddleServer(r.status, r.body)
		out := service(svcOpts{paddle: st}).Read(ctx, input(textPDF("Local fallback text works fine"), ""))
		if !strings.Contains(out.Text, "Local fallback text works fine") || out.Via != "pdf" {
			t.Errorf("reply %d %s: %+v", r.status, r.body, out)
		}
		if st.count("POST") == 0 {
			t.Errorf("reply %d: not sent", r.status)
		}
	}
}

func TestFailureNamesPaddleAndTheLocalReason(t *testing.T) {
	out := service(svcOpts{local: config.LocalOff, paddle: paddleServer(503, "")}).Read(ctx, input(textPDF("x"), ""))
	if out.Text != "" || out.Error != "PaddleOCR failed (HTTP 503); document reading is turned off" {
		t.Fatalf("%+v", out)
	}
	unreachable := service(svcOpts{local: config.LocalOff, paddle: down()}).Read(ctx, input(textPDF("x"), ""))
	if unreachable.Error != "PaddleOCR unreachable; document reading is turned off" {
		t.Fatalf("%+v", unreachable)
	}
	server := service(svcOpts{local: config.LocalOff, paddle: paddleServer(200, `{"errorCode":7}`)}).Read(ctx, input(textPDF("x"), ""))
	if server.Error != "PaddleOCR failed (error 7); document reading is turned off" {
		t.Fatalf("%+v", server)
	}
}

func TestReachabilityProbeIsCachedForAMinute(t *testing.T) {
	st := down()
	s := service(svcOpts{paddle: st})
	for _, text := range []string{"First document with enough text", "Second document with enough text", "Third document with enough text"} {
		s.Read(ctx, input(textPDF(text), ""))
	}
	if n := len(st.requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestProbeTTLExpires(t *testing.T) {
	st := stub(func(seenRequest) (int, string, error) { return 405, "", nil })
	p := newPaddle(paddleURL, "", st.client())
	t0 := time.Now()
	now := t0
	p.now = func() time.Time { return now }
	if !p.reachable(ctx) {
		t.Fatal("unreachable")
	}
	now = t0.Add(30 * time.Second)
	if !p.reachable(ctx) || len(st.requests()) != 1 {
		t.Fatalf("%d requests", len(st.requests()))
	}
	now = t0.Add(61 * time.Second)
	if !p.reachable(ctx) || len(st.requests()) != 2 {
		t.Fatalf("%d requests", len(st.requests()))
	}
}

func TestGatewayErrorOnProbeMeansUnreachable(t *testing.T) {
	st := stub(func(seenRequest) (int, string, error) { return 502, "", nil })
	if newPaddle(paddleURL, "", st.client()).reachable(ctx) {
		t.Fatal("reachable")
	}
}

func TestDeadPaddleOnPostIsSkippedForAMinute(t *testing.T) {
	var gets atomic.Int32
	st := stub(func(r seenRequest) (int, string, error) {
		if r.method == "GET" {
			gets.Add(1)
			return 405, "", nil
		}
		return 0, "", errors.New("connection reset")
	})
	s := service(svcOpts{paddle: st})
	s.Read(ctx, input(textPDF("First document with enough text"), ""))
	s.Read(ctx, input(textPDF("Second document with enough text"), ""))
	if st.count("POST") != 1 || gets.Load() != 1 {
		t.Fatalf("%d posts, %d probes", st.count("POST"), gets.Load())
	}
}

// MARK: Cache and cap

func TestResentDocumentsHitTheCache(t *testing.T) {
	st := paddleServer(200, paddleOK)
	s := service(svcOpts{paddle: st})
	pdf := textPDF("cached")
	a := s.Read(ctx, hooks.DocumentInput{Data: pdf, MediaType: "application/pdf", Title: "one"})
	b := s.Read(ctx, hooks.DocumentInput{Data: pdf, MediaType: "application/pdf", Title: "two"})
	if a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
	if st.count("POST") != 1 {
		t.Fatalf("%d posts", st.count("POST"))
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	var posts atomic.Int32
	st := stub(func(r seenRequest) (int, string, error) {
		if r.method != "POST" {
			return 405, "", nil
		}
		if posts.Add(1) == 1 {
			return 500, "", nil
		}
		return 200, paddleOK, nil
	})
	s := service(svcOpts{local: config.LocalOff, paddle: st})
	pdf := textPDF("x")
	if out := s.Read(ctx, input(pdf, "")); out.Error == "" {
		t.Fatalf("%+v", out)
	}
	if out := s.Read(ctx, input(pdf, "")); out.Via != "paddleocr" {
		t.Fatalf("%+v", out)
	}
}

func TestTextIsCappedOnACharacterBoundary(t *testing.T) {
	long := strings.Repeat("é", 5000) // 2 bytes each
	reply := `{"errorCode":0,"result":{"layoutParsingResults":[{"markdown":{"text":"` + long + `"}}]}}`
	out := service(svcOpts{paddle: paddleServer(200, reply), cap: 1001}).Read(ctx, input([]byte{1}, ""))
	body, _, _ := strings.Cut(out.Text, "\n\n[document truncated")
	if body != strings.Repeat("é", 500) {
		t.Fatalf("%d bytes", len(body))
	}
	if !strings.HasSuffix(out.Text, "[document truncated at 0 KB]") {
		t.Fatal(out.Text[len(out.Text)-40:])
	}
	if capped("short", 100) != "short" {
		t.Fatal("short")
	}
}

func TestDefaultCapIs600KB(t *testing.T) {
	if DefaultTextCap != 600*1024 || New(config.Documents{}, "", nil).textCap != DefaultTextCap {
		t.Fatal(DefaultTextCap)
	}
}

func TestLocalTextCapStopsReadingPages(t *testing.T) {
	ocr := &fakeOCR{text: "OCR text that is long enough to count"}
	var pages []string
	for range 5 {
		pages = append(pages, "Page text that is comfortably over twenty characters")
	}
	pages = append(pages, "") // a scan after the cap: never OCR'd
	out := service(svcOpts{ocr: ocr, cap: 60}).Read(ctx, input(textPDF(pages...), ""))
	if ocr.count() != 0 || !strings.HasSuffix(out.Text, "[document truncated at 0 KB]") || out.Pages != 6 {
		t.Fatalf("%+v, %d OCR calls", out, ocr.count())
	}
}
