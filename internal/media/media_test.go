package media_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/media"
	"codelink/internal/testkit"
	"codelink/internal/upstream"
)

// Fixtures and helpers.

var pdfData = base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf body")) // 22 bytes

const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}`

func parse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("bad JSON fixture: %v\n%s", err, s)
	}
	return v
}

func request(t *testing.T, messages string) map[string]any {
	t.Helper()
	return jsonx.Obj(parse(t, `{"model":"m","max_tokens":10,"messages":`+messages+`}`))
}

func messagesOf(req map[string]any) []any { return jsonx.Arr(req["messages"]) }

// texts are the text of every block of a content array.
func texts(content any) []string {
	var out []string
	for _, b := range jsonx.Arr(content) {
		out = append(out, jsonx.Str(jsonx.Get(b, "text")))
	}
	return out
}

func types(content any) []string {
	var out []string
	for _, b := range jsonx.Arr(content) {
		out = append(out, jsonx.Type(b))
	}
	return out
}

func eq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

// pagesReader is the Swift CoreDocumentReader: "EXTRACTED <n> bytes of
// <type>", 2 pages, 1 of them OCR'd; out overrides the answer.
type pagesReader struct {
	mu     sync.Mutex
	inputs []hooks.DocumentInput
	out    func(hooks.DocumentInput) hooks.DocumentOutput
}

func (r *pagesReader) Read(_ context.Context, in hooks.DocumentInput) hooks.DocumentOutput {
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	r.mu.Unlock()
	if r.out != nil {
		return r.out(in)
	}
	return hooks.DocumentOutput{Text: fmt.Sprintf("EXTRACTED %d bytes of %s", len(in.Data), in.MediaType), Pages: 2, OCRPages: 1, Via: "fake"}
}

func (r *pagesReader) Inputs() []hooks.DocumentInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hooks.DocumentInput(nil), r.inputs...)
}

// blankDescriber cannot describe anything.
type blankDescriber struct{}

func (blankDescriber) Describe(context.Context, []byte, string) string { return "  \n" }

func noVision(d hooks.ImageDescriber) media.Context {
	return media.Context{Describer: d, ModelID: "GLM", Fetcher: upstream.NewClient()}
}

// Anthropic Messages: documents (HookTests DocumentHookTests).

func TestDocumentsAreReadIntoText(t *testing.T) {
	reader := &pagesReader{}
	log := &testkit.Log{}
	req := request(t, `[{"role":"user","content":[{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"`+pdfData+`"}},{"type":"text","text":"summarise"}]}]`)
	before := jsonx.String(req)
	out := media.ProcessMessages(context.Background(), req, media.Context{Reader: reader, Log: log, ModelID: "GLM"})

	in := reader.Inputs()
	if len(in) != 1 || in[0].MediaType != "application/pdf" || in[0].Title != "report.pdf" || string(in[0].Data) != "%PDF-1.4 fake pdf body" {
		t.Fatalf("reader inputs: %+v", in)
	}
	content := jsonx.Get(messagesOf(out), 0, "content")
	eq(t, "texts", texts(content), []string{"[document: report.pdf]\nEXTRACTED 22 bytes of application/pdf", "summarise"})
	eq(t, "types", types(content), []string{"text", "text"})
	eq(t, "log", log.All(), []hooks.Event{{Kind: "document", Name: "report.pdf", Pages: 2, OCRPages: 1}})
	if jsonx.String(req) != before {
		t.Error("the request was modified in place")
	}
}

func TestDocumentsInToolResultsBecomeAttachmentText(t *testing.T) {
	reader := &testkit.DocReader{}
	req := request(t, `[{"role":"user","content":"read a.pdf"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"a.pdf"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"`+pdfData+`"}}]},{"type":"text","text":"go on"}]}]`)
	out := messagesOf(media.ProcessMessages(context.Background(), req, media.Context{Reader: reader}))

	eq(t, "untouched messages", out[:2], messagesOf(req)[:2])
	last := jsonx.Get(out, 2, "content")
	eq(t, "types", types(last), []string{"tool_result", "text"})
	tr := jsonx.Get(last, 0)
	eq(t, "tool_use_id", jsonx.Str(jsonx.Get(tr, "tool_use_id")), "toolu_1")
	eq(t, "tool result types", types(jsonx.Get(tr, "content")), []string{media.AttachmentTextType})
	eq(t, "tool result texts", texts(jsonx.Get(tr, "content")), []string{"[document: document]\nEXTRACTED 22 bytes of application/pdf"})
	eq(t, "go on", jsonx.Str(jsonx.Get(last, 1, "text")), "go on")
}

func TestUnreadableDocumentsBecomeNotes(t *testing.T) {
	failing := &pagesReader{out: func(hooks.DocumentInput) hooks.DocumentOutput {
		return hooks.DocumentOutput{Error: "no text layer", Via: "pdfkit"}
	}}
	doc := `{"type":"document","title":"scan.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfData + `"}}`
	one := func(mc media.Context, messages string) []string {
		return texts(jsonx.Get(messagesOf(media.ProcessMessages(context.Background(), request(t, messages), mc)), 0, "content"))
	}
	eq(t, "reader error", one(media.Context{Reader: failing}, `[{"role":"user","content":[`+doc+`]}]`),
		[]string{"[document: scan.pdf could not be read (no text layer)]"})
	eq(t, "bad base64", one(media.Context{Reader: failing}, `[{"role":"user","content":[{"type":"document","title":"bad.pdf","source":{"type":"base64","media_type":"application/pdf","data":"@@@"}}]}]`),
		[]string{"[document: bad.pdf could not be read (invalid base64)]"})
	eq(t, "no reader", one(media.Context{}, `[{"role":"user","content":[`+doc+`]}]`),
		[]string{"[document: scan.pdf could not be read (document reading is not available)]"})
	empty := &pagesReader{out: func(hooks.DocumentInput) hooks.DocumentOutput { return hooks.DocumentOutput{Text: " \n "} }}
	eq(t, "no text", one(media.Context{Reader: empty}, `[{"role":"user","content":[`+doc+`]}]`),
		[]string{"[document: scan.pdf could not be read (no text found in it)]"})
	big := base64.StdEncoding.EncodeToString(make([]byte, 2<<20))
	eq(t, "too large", one(media.Context{Reader: empty, MaxFetchBytes: 1 << 20}, `[{"role":"user","content":[{"type":"document","title":"big.pdf","source":{"type":"base64","media_type":"application/pdf","data":"`+big+`"}}]}]`),
		[]string{"[document: big.pdf could not be read (too large, over 1 MiB)]"})
	if len(empty.Inputs()) != 1 {
		t.Errorf("a document over the cap was read: %d reads", len(empty.Inputs()))
	}
}

func TestTextContentAndURLSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/files/doc.pdf" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, "%PDF-1.7 from url")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	reader := &testkit.DocReader{}
	txt := base64.StdEncoding.EncodeToString([]byte("plain notes"))
	req := request(t, `[{"role":"user","content":[`+
		`{"type":"document","title":"a.txt","source":{"type":"text","media_type":"text/plain","data":"alpha"}},`+
		`{"type":"document","title":"b","context":"from the wiki","source":{"type":"content","content":[{"type":"text","text":"beta"}]}},`+
		`{"type":"document","title":"c.txt","source":{"type":"base64","media_type":"text/plain","data":"`+txt+`"}},`+
		`{"type":"document","title":"d.pdf","source":{"type":"url","url":"`+srv.URL+`/files/doc.pdf"}},`+
		`{"type":"document","title":"e","source":{"type":"url","url":"ftp://x/y"}}]}]`)
	out := media.ProcessMessages(context.Background(), req, media.Context{Reader: reader, Fetcher: upstream.NewClient()})
	eq(t, "texts", texts(jsonx.Get(messagesOf(out), 0, "content")), []string{
		"[document: a.txt]\nalpha",
		"[document: b]\nfrom the wiki",
		"beta",
		"[document: c.txt]\nplain notes",
		"[document: d.pdf]\nEXTRACTED 17 bytes of application/json",
		"[document: e could not be read (only http and https URLs are fetched)]",
	})
}

// Swift returned the request untouched when it had no reads to run, so a
// request whose only documents were inline (text / content sources) left
// them for the translator's "unsupported document source" note. Here
// they become text whatever else the request holds.
func TestInlineDocumentsAloneBecomeText(t *testing.T) {
	req := request(t, `[{"role":"user","content":[{"type":"document","name":" notes.md ","source":{"type":"text","data":"alpha"}},`+
		`{"type":"document","source":{"type":"content","content":"gamma"}}]}]`)
	out := media.ProcessMessages(context.Background(), req, media.Context{})
	eq(t, "texts", texts(jsonx.Get(messagesOf(out), 0, "content")), []string{"[document: notes.md]\nalpha", "[document: document]\ngamma"})
}

func TestContentSourceImagesAreProcessedAndOtherBlocksDropped(t *testing.T) {
	req := request(t, `[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Fetch","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[`+
		`{"type":"document","title":"page","source":{"type":"content","content":[{"type":"text","text":"one"},`+image+`,{"type":"search_result"}]}}]}]}]`)
	describer := &testkit.Describer{}
	out := messagesOf(media.ProcessMessages(context.Background(), req, noVision(describer)))
	inner := jsonx.Get(out, 1, "content", 0, "content")
	eq(t, "types", types(inner), []string{media.AttachmentTextType, media.AttachmentTextType, media.AttachmentTextType})
	eq(t, "texts", texts(inner), []string{"[document: page]", "one", "[image: a cat on a mat]"})
}

func TestDocumentURLFetchFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write(make([]byte, 1<<20+1))
		case "/hop":
			http.Redirect(w, r, "/notes.txt", http.StatusFound)
		case "/notes.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "fetched notes")
		case "/bare":
			w.Header()["Content-Type"] = nil
			w.Write([]byte{0x00, 0x01})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	reader := &testkit.DocReader{}
	doc := func(title, path string) string {
		return `{"type":"document","title":"` + title + `","context":"ctx","source":{"type":"url","url":"` + srv.URL + path + `"}}`
	}
	req := request(t, `[{"role":"user","content":[`+doc("missing", "/nope")+`,`+doc("big", "/big")+`,`+doc("hop", "/hop")+`,`+doc("notes", "/notes.txt")+`,`+doc("bare", "/bare")+`]}]`)
	out := media.ProcessMessages(context.Background(), req, media.Context{Reader: reader, Fetcher: upstream.NewClient(), MaxFetchBytes: 1 << 20})
	got := texts(jsonx.Get(messagesOf(out), 0, "content"))
	eq(t, "404", got[0], "[document: missing could not be read (HTTP 404)]")
	eq(t, "too large", got[1], "[document: big could not be read (too large, over 1 MiB)]")
	// The redirect is to the same loopback server: refused, and the
	// address is scrubbed from the note.
	if !strings.HasPrefix(got[2], "[document: hop could not be read (could not fetch it: ") ||
		!strings.Contains(got[2], "redirect to a private address refused") || strings.Contains(got[2], "127.0.0.1") {
		t.Errorf("redirect note: %q", got[2])
	}
	eq(t, "text/plain", got[3], "[document: notes]\nctx\nfetched notes")
	eq(t, "no content type", got[4], "[document: bare]\nctx\nEXTRACTED 2 bytes of application/octet-stream")
}

// Anthropic Messages: images (HookTests ImageHookTests).

func TestImagesDescribedForAModelWithoutVision(t *testing.T) {
	describer := &testkit.Describer{}
	req := request(t, `[{"role":"user","content":[{"type":"text","text":"what is it"},`+image+`,`+image+`]}]`)
	out := media.ProcessMessages(context.Background(), req, noVision(describer))
	content := jsonx.Get(messagesOf(out), 0, "content")
	eq(t, "texts", texts(content), []string{"what is it", "[image: a cat on a mat]", "[image: a cat on a mat]"})
	eq(t, "types", types(content), []string{"text", "text", "text"})
	eq(t, "describe calls", describer.Calls(), 1) // the same image once
}

func TestImagesOmittedWithoutAVisionRole(t *testing.T) {
	req := request(t, `[{"role":"user","content":[`+image+`]}]`)
	out := media.ProcessMessages(context.Background(), req, noVision(nil))
	eq(t, "texts", texts(jsonx.Get(messagesOf(out), 0, "content")),
		[]string{"[image omitted: GLM has no vision and no vision model is configured]"})
}

func TestImagesPassedToAVisionModel(t *testing.T) {
	describer := &testkit.Describer{}
	req := request(t, `[{"role":"user","content":[`+image+`]},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Shot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[`+image+`]}]}]`)
	before := jsonx.String(req)
	mc := noVision(describer)
	mc.ModelHasVision = true
	out := media.ProcessMessages(context.Background(), req, mc)
	eq(t, "unchanged", jsonx.String(out), before)
	eq(t, "describe calls", describer.Calls(), 0)
}

func TestImagesDescribedInsideToolResults(t *testing.T) {
	describer := &testkit.Describer{Text: "a red button"}
	req := request(t, `[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Shot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"shot taken"},`+image+`]}]}]`)
	out := messagesOf(media.ProcessMessages(context.Background(), req, noVision(describer)))
	inner := jsonx.Get(out, 1, "content", 0, "content")
	eq(t, "types", types(inner), []string{"text", media.AttachmentTextType})
	eq(t, "texts", texts(inner), []string{"shot taken", "[image: a red button]"})
}

func TestImageNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cat.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	urlImage := func(u string) string { return `{"type":"image","source":{"type":"url","url":"` + u + `"}}` }
	req := request(t, `[{"role":"user","content":[`+urlImage(srv.URL+"/cat.png")+`,`+urlImage(srv.URL+"/gone.png")+`,`+urlImage("file:///etc/passwd")+`]}]`)
	describer := &testkit.Describer{}
	got := texts(jsonx.Get(messagesOf(media.ProcessMessages(context.Background(), req, noVision(describer))), 0, "content"))
	eq(t, "texts", got, []string{"[image: a cat on a mat]", "[image omitted: it could not be fetched]", "[image omitted: it could not be fetched]"})
	eq(t, "describe calls", describer.Calls(), 1)

	blank := request(t, `[{"role":"user","content":[`+image+`]}]`)
	got = texts(jsonx.Get(messagesOf(media.ProcessMessages(context.Background(), blank, noVision(blankDescriber{}))), 0, "content"))
	eq(t, "undescribed", got, []string{"[image omitted: the vision model could not describe it]"})
}

func TestOnlyUserMessagesAndKnownSourcesAreTouched(t *testing.T) {
	req := request(t, `[{"role":"assistant","content":[`+image+`]},{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"f1"}},{"type":"image","source":{"type":"file","file_id":"f2"}}]}]`)
	before := jsonx.String(req)
	out := media.ProcessMessages(context.Background(), req, noVision(&testkit.Describer{}))
	eq(t, "unchanged", jsonx.String(out), before)
	noMessages := map[string]any{"model": "m"}
	eq(t, "no messages", media.ProcessMessages(context.Background(), noMessages, media.Context{}), noMessages)
}

func TestReadsAreDedupedAndRunFourAtATime(t *testing.T) {
	var active, peak atomic.Int32
	reader := &pagesReader{out: func(in hooks.DocumentInput) hooks.DocumentOutput {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		return hooks.DocumentOutput{Text: "read " + in.Title}
	}}
	var blocks []string
	for i := range 10 {
		data := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%%PDF doc %d", i%8)))
		blocks = append(blocks, `{"type":"document","title":"d`+fmt.Sprint(i%8)+`","source":{"type":"base64","media_type":"application/pdf","data":"`+data+`"}}`)
	}
	req := request(t, `[{"role":"user","content":[`+strings.Join(blocks, ",")+`]}]`)
	got := texts(jsonx.Get(messagesOf(media.ProcessMessages(context.Background(), req, media.Context{Reader: reader})), 0, "content"))
	if len(reader.Inputs()) != 8 {
		t.Errorf("reads: got %d, want 8 (one per distinct document)", len(reader.Inputs()))
	}
	if p := peak.Load(); p != 4 {
		t.Errorf("peak concurrency: got %d, want 4", p)
	}
	eq(t, "order kept", got[9], "[document: d1]\nread d1")
}

// Responses input (ResponsesTests documents and images).

func TestResponsesInputFilePDFsGoThroughTheDocumentReader(t *testing.T) {
	reader := &testkit.DocReader{}
	log := &testkit.Log{}
	pdf := "JVBERi0xLjQK"
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[
	  {"type":"input_text","text":"read these"},
	  {"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,`+pdf+`"},
	  {"type":"input_file","filename":"b.pdf","file_data":"`+pdf+`"},
	  {"type":"input_file","filename":"c.txt","file_data":"data:text/plain;base64,aGkgdGhlcmU="},
	  {"type":"input_file","file_id":"file-123"},
	  {"type":"input_image","image_url":"data:application/pdf;base64,`+pdf+`"}]}]`))
	out := media.ProcessResponsesInput(context.Background(), items, media.Context{Reader: reader, Log: log})
	content := jsonx.Get(out, 0, "content")
	eq(t, "types", types(content), []string{"input_text", "input_text", "input_text", "input_text", "input_text", "input_text"})
	got := texts(content)
	eq(t, "text", got[0], "read these")
	eq(t, "a.pdf", got[1], "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf")
	eq(t, "b.pdf", got[2], "[document: b.pdf]\nEXTRACTED 9 bytes of application/pdf")
	eq(t, "c.txt", got[3], "[document: c.txt]\nhi there")
	eq(t, "file id", got[4], `[attached file "unnamed file" could not be read: only inline file_data is supported]`)
	if !strings.HasPrefix(got[5], "[document: document]\nEXTRACTED") {
		t.Errorf("PDF image: %q", got[5])
	}
	// The same PDF is read once per distinct name.
	in := reader.Calls()
	if len(in) != 3 {
		t.Fatalf("reads: %d, want 3", len(in))
	}
	for _, c := range in {
		if c.MediaType != "application/pdf" {
			t.Errorf("media type %q", c.MediaType)
		}
	}
	found := false
	for _, e := range log.All() {
		found = found || (e.Kind == "document" && e.Name == "a.pdf")
	}
	if !found {
		t.Errorf("no document event for a.pdf: %+v", log.All())
	}
}

func TestResponsesDocumentsWithoutAReaderAreNotedNotDropped(t *testing.T) {
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]`))
	out := media.ProcessResponsesInput(context.Background(), items, media.Context{})
	eq(t, "texts", texts(jsonx.Get(out, 0, "content")), []string{"[document: a.pdf could not be read (document reading is not available)]"})
}

func TestResponsesImagesForAModelWithoutVisionAreDescribed(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(make([]byte, 64))
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"input_text","text":"what is this"},
	   {"type":"input_image","image_url":"data:image/png;base64,`+png+`"}]},
	 {"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
	 {"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,`+png+`"}]},
	 {"type":"custom_tool_call_output","call_id":"c2","output":{"content":[{"type":"input_text","text":"done"},{"type":"input_image","image_url":{"url":"data:image/png;base64,`+png+`"}}]}}]`))
	describer := &testkit.Describer{}
	out := media.ProcessResponsesInput(context.Background(), items, noVision(describer))
	eq(t, "message types", types(jsonx.Get(out, 0, "content")), []string{"input_text", "input_text"})
	eq(t, "message texts", texts(jsonx.Get(out, 0, "content")), []string{"what is this", "[image: a cat on a mat]"})
	eq(t, "call", out[1], items[1])
	eq(t, "output types", types(jsonx.Get(out, 2, "output")), []string{media.AttachmentTextType})
	eq(t, "output texts", texts(jsonx.Get(out, 2, "output")), []string{"[image: a cat on a mat]"})
	eq(t, "output.content types", types(jsonx.Get(out, 3, "output", "content")), []string{"input_text", media.AttachmentTextType})
	eq(t, "output.content texts", texts(jsonx.Get(out, 3, "output", "content")), []string{"done", "[image: a cat on a mat]"})
	eq(t, "describe calls", describer.Calls(), 1) // the same image is described once
}

func TestResponsesImagesWithoutAnyVisionAreOmittedWithANote(t *testing.T) {
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_image","file_id":"file-1"}]}]`))
	out := media.ProcessResponsesInput(context.Background(), items, noVision(nil))
	eq(t, "texts", texts(jsonx.Get(out, 0, "content")), []string{
		"[image omitted: GLM has no vision and no vision model is configured]",
		"[image omitted: it is attached by file id, which code-link cannot read]",
	})
}

func TestResponsesImagesStayForAModelWithVision(t *testing.T) {
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]`))
	describer := &testkit.Describer{}
	mc := noVision(describer)
	mc.ModelHasVision = true
	out := media.ProcessResponsesInput(context.Background(), items, mc)
	eq(t, "unchanged", jsonx.String(out), jsonx.String(items))
	eq(t, "describe calls", describer.Calls(), 0)
}

func TestResponsesFileURLsAreFetched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		fmt.Fprint(w, "%PDF-1.4")
	}))
	defer srv.Close()
	items := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"input_file","filename":" r.pdf ","file_url":"`+srv.URL+`/r.pdf"}]}]`))
	out := media.ProcessResponsesInput(context.Background(), items, media.Context{Reader: &testkit.DocReader{}, Fetcher: upstream.NewClient()})
	eq(t, "texts", texts(jsonx.Get(out, 0, "content")), []string{"[document: r.pdf]\nEXTRACTED 8 bytes of application/pdf"})
}

// Chat Completions messages (ChatCompletionsTests documents and images).

func chatMessages(t *testing.T, png string) []any {
	return jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"text","text":"what is this"},
	   {"type":"image_url","image_url":{"url":"data:image/png;base64,`+png+`","detail":"low"}}]}]`))
}

func TestChatImagesForAModelWithoutVisionAreDescribedOrNoted(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(make([]byte, 64))
	describer := &testkit.Describer{}
	out := media.ProcessChatMessages(context.Background(), chatMessages(t, png), noVision(describer))
	parts := jsonx.Get(out, 0, "content")
	eq(t, "types", types(parts), []string{"text", "text"})
	eq(t, "described", texts(parts)[1], "[image: a cat on a mat]")
	eq(t, "describe calls", describer.Calls(), 1)

	out = media.ProcessChatMessages(context.Background(), chatMessages(t, png), noVision(nil))
	eq(t, "omitted", texts(jsonx.Get(out, 0, "content"))[1], "[image omitted: GLM has no vision and no vision model is configured]")

	// A vision model keeps its images untouched.
	mc := noVision(nil)
	mc.ModelHasVision = true
	in := chatMessages(t, png)
	out = media.ProcessChatMessages(context.Background(), in, mc)
	eq(t, "kept", jsonx.String(out), jsonx.String(in))
}

func TestChatPDFFilePartsAreReadAsDocuments(t *testing.T) {
	reader := &testkit.DocReader{}
	messages := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"text","text":"summarise"},
	   {"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}},
	   {"type":"file","file":{"file_id":"file-9"}},
	   {"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0xLjQK"}}]},
	 {"role":"tool","tool_call_id":"c","content":[{"type":"file","file":{"filename":"notes.txt","file_data":"aGkgdGhlcmU="}}]}]`))
	mc := media.Context{Reader: reader, ModelHasVision: true}
	out := media.ProcessChatMessages(context.Background(), messages, mc)
	parts := jsonx.Get(out, 0, "content")
	eq(t, "types", types(parts), []string{"text", "text", "text", "text"})
	got := texts(parts)
	eq(t, "report.pdf", got[1], "[document: report.pdf]\nEXTRACTED 9 bytes of application/pdf")
	eq(t, "file id", got[2], `[attached file "unnamed file" could not be read: only inline file_data is supported]`)
	if !strings.HasPrefix(got[3], "[document: document]") {
		t.Errorf("PDF image: %q", got[3])
	}
	eq(t, "tool file", texts(jsonx.Get(out, 1, "content")), []string{"[document: notes.txt]\nhi there"})
	eq(t, "tool file type", types(jsonx.Get(out, 1, "content")), []string{"text"})
	in := reader.Calls()
	if len(in) != 2 || (in[0].Title != "report.pdf" && in[1].Title != "report.pdf") {
		t.Errorf("reads: %+v", in)
	}
}

// ChatCompletionsTests.imagesGoToAnAnthropicModelAndDocumentsAreRead: the
// media half (the Anthropic translation is the chat package's).
func TestChatImagesStayAndDocumentsAreReadForAVisionModel(t *testing.T) {
	messages := jsonx.Arr(parse(t, `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/"}},
	  {"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]`))
	out := media.ProcessChatMessages(context.Background(), messages, media.Context{Reader: &testkit.DocReader{}, ModelHasVision: true})
	parts := jsonx.Get(out, 0, "content")
	eq(t, "types", types(parts), []string{"image_url", "text"})
	eq(t, "image", jsonx.Get(parts, 0), jsonx.Get(messages, 0, "content", 0))
	eq(t, "document", texts(parts)[1], "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf")
}

func TestDataURL(t *testing.T) {
	for _, c := range []struct {
		in, mt, data string
		ok           bool
	}{
		{"data:image/PNG;base64,AAAA", "image/png", "AAAA", true},
		{"data:application/pdf; name=a.pdf ;BASE64,JVBERi", "application/pdf", "JVBERi", true},
		{"data:;base64,QQ==", "application/octet-stream", "QQ==", true},
		{"data:base64,QQ==", "application/octet-stream", "QQ==", true},
		{"data:text/plain,hello", "", "", false},
		{"data:image/png;base64", "", "", false},
		{"https://x/y.png", "", "", false},
	} {
		mt, data, ok := media.DataURL(c.in)
		if mt != c.mt || data != c.data || ok != c.ok {
			t.Errorf("DataURL(%q) = %q, %q, %v; want %q, %q, %v", c.in, mt, data, ok, c.mt, c.data, c.ok)
		}
	}
}
