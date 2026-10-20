// Package media rewrites a request's documents and images before
// translation (docs/DESIGN.md §4.4): documents become text through the
// DocumentReader, images for a model without vision become descriptions
// (or an "image omitted" note). Reads run concurrently, once per distinct
// document or image in the request.
//
// Port of the Swift MediaPreprocessor.swift and OpenAIMedia.swift.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"codelink/internal/apierr"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/upstream"
)

// AttachmentTextType marks a text block that came from media inside a
// tool_result: the Messages translator puts it in the user message after
// the tool messages.
const AttachmentTextType = "code_link_attachment_text"

// Context is what media processing needs for one request.
type Context struct {
	Reader hooks.DocumentReader // nil = documents become a note
	// Describer is set only when the vision role names a model.
	Describer      hooks.ImageDescriber
	ModelHasVision bool
	ModelID        string
	Fetcher        *upstream.Client // nil = a shared default client
	Log            hooks.EventLog   // nil = no events
	MaxFetchBytes  int64            // 0 = 32 MiB
}

// concurrency is how many reads run at once.
const concurrency = 4

// fetchHeaderTimeout bounds the wait for a fetched URL's response headers.
const fetchHeaderTimeout = 60 * time.Second

var defaultFetcher = sync.OnceValue(upstream.NewClient)

func (mc Context) maxBytes() int64 {
	if mc.MaxFetchBytes > 0 {
		return mc.MaxFetchBytes
	}
	return 32 << 20
}

// source is where a document's or image's bytes are.
type source struct {
	url       string // set for a URL source
	data      string // base64 otherwise
	mediaType string
}

// job is one read: a document (through the Reader) or an image (through
// the Describer).
type job struct {
	document bool
	src      source
	title    string // documents: trimmed title or name, "" = none
	context  string // documents: the block's context, "" = none
}

func docJob(src source, title, context string) job {
	return job{document: true, src: src, title: title, context: context}
}

func imageJob(src source) job { return job{src: src} }

// ProcessMessages rewrites an Anthropic Messages request's document and
// image blocks (top level and inside tool_result). Documents whose text is
// in the request (text and content sources, base64 text/*) become text
// directly; base64 and URL documents go through the Reader; images, for a
// model without vision, become a description or an "image omitted" note.
// Media inside a tool_result becomes AttachmentTextType blocks. req is not
// modified; the result shares its unchanged parts.
func ProcessMessages(ctx context.Context, req map[string]any, mc Context) map[string]any {
	messages, ok := req["messages"].([]any)
	if !ok {
		return req
	}
	jobs := map[string]job{}
	expanded := false
	rewriteMessages(messages, &expanded, func(b jsonx.Object, _ bool) []any {
		if key, j, ok := messageJob(b, mc); ok {
			jobs[key] = j
		}
		return nil
	})
	if len(jobs) == 0 && !expanded {
		return req
	}
	results := runAll(ctx, jobs, mc)
	rewritten := rewriteMessages(messages, &expanded, func(b jsonx.Object, inToolResult bool) []any {
		key, _, ok := messageJob(b, mc)
		if !ok {
			return nil
		}
		text, ok := results[key]
		if !ok {
			return nil
		}
		return []any{textBlock(textType(inToolResult), text)}
	})
	out := maps.Clone(req)
	out["messages"] = rewritten
	return out
}

func textType(inToolResult bool) string {
	if inToolResult {
		return AttachmentTextType
	}
	return "text"
}

func textBlock(typ string, text any) jsonx.Object {
	return jsonx.Object{"type": typ, "text": text}
}

// rewriteMessages applies replace to every block of every user message
// (and to the blocks inside tool_result content); nil keeps the block.
// Inline documents are expanded here, which sets *expanded.
func rewriteMessages(messages []any, expanded *bool, replace func(jsonx.Object, bool) []any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		mo, ok := m.(jsonx.Object)
		blocks, isArr := mo["content"].([]any)
		if !ok || jsonx.Str(mo["role"]) != "user" || !isArr {
			out[i] = m
			continue
		}
		nm := maps.Clone(mo)
		nm["content"] = rewriteBlocks(blocks, false, expanded, replace)
		out[i] = nm
	}
	return out
}

func rewriteBlocks(blocks []any, inToolResult bool, expanded *bool, replace func(jsonx.Object, bool) []any) []any {
	out := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		b, ok := raw.(jsonx.Object)
		if !ok {
			out = append(out, raw)
			continue
		}
		typ := jsonx.Type(b)
		if inner, isArr := b["content"].([]any); typ == "tool_result" && !inToolResult && isArr {
			nb := maps.Clone(b)
			nb["content"] = rewriteBlocks(inner, true, expanded, replace)
			out = append(out, nb)
			continue
		}
		if typ == "document" {
			if inline, ok := inlineDocument(b, inToolResult); ok {
				*expanded = true
				out = append(out, rewriteBlocks(inline, inToolResult, expanded, replace)...)
				continue
			}
		}
		if r := replace(b, inToolResult); r != nil {
			out = append(out, r...)
		} else {
			out = append(out, raw)
		}
	}
	return out
}

// firstStr is the first of vs that is a string.
func firstStr(vs ...any) (string, bool) {
	for _, v := range vs {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// trimSpaces trims spaces and tabs (not newlines).
func trimSpaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// header is "[document: <title>]\n" plus the block's context line.
func header(b jsonx.Object) string {
	title, _ := firstStr(b["title"], b["name"])
	title = trimSpaces(title)
	if title == "" {
		title = "document"
	}
	h := "[document: " + title + "]\n"
	if c := jsonx.Str(b["context"]); c != "" {
		h += c + "\n"
	}
	return h
}

// inlineDocument expands documents whose text is in the request: text and
// content sources, and base64 text/*.
func inlineDocument(b jsonx.Object, inToolResult bool) ([]any, bool) {
	src := jsonx.Obj(b["source"])
	tt := textType(inToolResult)
	switch jsonx.Type(src) {
	case "text":
		return []any{textBlock(tt, header(b)+jsonx.Str(src["data"]))}, true
	case "content":
		if s, ok := src["content"].(string); ok {
			return []any{textBlock(tt, header(b)+s)}, true
		}
		parts := []any{textBlock(tt, strings.TrimSuffix(header(b), "\n"))}
		for _, raw := range jsonx.Arr(src["content"]) {
			ib := jsonx.Obj(raw)
			switch jsonx.Type(ib) {
			case "text":
				text := ib["text"]
				if text == nil {
					text = ""
				}
				parts = append(parts, textBlock(tt, text))
			case "image":
				parts = append(parts, ib)
			}
		}
		return parts, true
	case "base64":
		mt := strings.ToLower(jsonx.Str(src["media_type"]))
		if strings.HasPrefix(mt, "text/") {
			if d, err := base64.StdEncoding.DecodeString(jsonx.Str(src["data"])); err == nil {
				return []any{textBlock(tt, header(b)+string(d))}, true
			}
		}
	}
	return nil, false
}

// sha is the hex SHA-256 of s.
func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// messageJob is the read an Anthropic block needs, keyed so the same
// document or image is read once per request.
func messageJob(b jsonx.Object, mc Context) (string, job, bool) {
	src := jsonx.Obj(b["source"])
	switch jsonx.Type(b) {
	case "document":
		t, _ := firstStr(b["title"], b["name"])
		title := trimSpaces(t)
		context := jsonx.Str(b["context"])
		switch jsonx.Type(src) {
		case "base64":
			data := jsonx.Str(src["data"])
			mt, ok := src["media_type"].(string)
			if !ok {
				mt = "application/pdf"
			}
			return "doc|" + title + "|" + context + "|" + mt + "|" + sha(data),
				docJob(source{data: data, mediaType: mt}, title, context), true
		case "url":
			u := jsonx.Str(src["url"])
			return "docurl|" + title + "|" + context + "|" + u, docJob(source{url: u}, title, context), true
		}
		return "", job{}, false // the translator notes it
	case "image":
		if mc.ModelHasVision {
			return "", job{}, false
		}
		switch jsonx.Type(src) {
		case "base64":
			data := jsonx.Str(src["data"])
			mt, ok := src["media_type"].(string)
			if !ok {
				mt = "image/png"
			}
			return "img|" + mt + "|" + sha(data), imageJob(source{data: data, mediaType: mt}), true
		case "url":
			u := jsonx.Str(src["url"])
			return "imgurl|" + u, imageJob(source{url: u}), true
		}
	}
	return "", job{}, false
}

// runAll runs the jobs, four at a time.
func runAll(ctx context.Context, jobs map[string]job, mc Context) map[string]string {
	results := make(map[string]string, len(jobs))
	if len(jobs) == 0 {
		return results
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for key, j := range jobs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			text := run(ctx, j, mc)
			mu.Lock()
			results[key] = text
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}

func run(ctx context.Context, j job, mc Context) string {
	if !j.document {
		return runImage(ctx, j, mc)
	}
	name := j.title
	if name == "" {
		name = "document"
	}
	note := func(why string) string { return "[document: " + name + " could not be read (" + why + ")]" }
	withHeader := func(text string) string {
		h := "[document: " + name + "]\n"
		if j.context != "" {
			h += j.context + "\n"
		}
		return h + text
	}
	data, mt, err := load(ctx, j.src, mc)
	if err != nil {
		return note(err.Error())
	}
	if strings.HasPrefix(strings.ToLower(mt), "text/") {
		return withHeader(string(data))
	}
	if mc.Reader == nil {
		return note("document reading is not available")
	}
	out := mc.Reader.Read(ctx, hooks.DocumentInput{Data: data, MediaType: mt, Title: j.title})
	if mc.Log != nil {
		mc.Log.Record(hooks.Event{Kind: "document", Name: name, Pages: out.Pages, OCRPages: out.OCRPages})
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		why := out.Error
		if why == "" {
			why = "no text found in it"
		}
		return note(why)
	}
	return withHeader(text)
}

func runImage(ctx context.Context, j job, mc Context) string {
	if mc.Describer == nil {
		return "[image omitted: " + mc.ModelID + " has no vision and no vision model is configured]"
	}
	data, mt, err := load(ctx, j.src, mc)
	if err != nil {
		return "[image omitted: it could not be fetched]"
	}
	d := strings.TrimSpace(mc.Describer.Describe(ctx, data, mt))
	if d == "" {
		return "[image omitted: the vision model could not describe it]"
	}
	return "[image: " + d + "]"
}

// loadError is why a source could not be loaded, as the note says it.
type loadError string

func (e loadError) Error() string { return string(e) }

// load gives a source's bytes and media type.
func load(ctx context.Context, src source, mc Context) ([]byte, string, error) {
	max := mc.maxBytes()
	tooLarge := loadError(fmt.Sprintf("too large, over %d MiB", max>>20))
	if src.url == "" {
		if int64(len(src.data)/4*3) > max {
			return nil, "", tooLarge
		}
		d, ok := decodeLenient(src.data)
		if !ok {
			return nil, "", loadError("invalid base64")
		}
		return d, src.mediaType, nil
	}
	u, err := url.Parse(src.url)
	if err != nil || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
		return nil, "", loadError("only http and https URLs are fetched")
	}
	fetcher := mc.Fetcher
	if fetcher == nil {
		fetcher = defaultFetcher()
	}
	resp, err := fetcher.Send(ctx, upstream.Request{URL: src.url, Method: "GET", RefusePrivateRedirects: true}, fetchHeaderTimeout)
	if err != nil {
		return nil, "", loadError("could not fetch it: " + apierr.Scrub(upstream.Describe(err)))
	}
	if resp.Status < 200 || resp.Status >= 300 {
		resp.Close()
		return nil, "", loadError(fmt.Sprintf("HTTP %d", resp.Status))
	}
	data, err := resp.Collect(max)
	if errors.Is(err, upstream.ErrBodyTooLarge) {
		return nil, "", tooLarge
	}
	if err != nil {
		return nil, "", loadError("could not fetch it: " + apierr.Scrub(upstream.Describe(err)))
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if mt == "" {
		mt = "application/octet-stream"
	}
	return data, mt, nil
}

// decodeLenient decodes base64, ignoring characters outside the alphabet
// (line breaks and the like) and missing padding. Input that holds no
// base64 at all is invalid.
func decodeLenient(s string) ([]byte, bool) {
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' {
			return r
		}
		return -1
	}, s)
	d, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil || (len(d) == 0 && s != "") {
		return nil, false
	}
	return d, true
}

// OpenAI-shaped requests (Responses input items, Chat Completions
// messages): the same jobs, readers and notes.

type dialect int

const (
	dialectResponses dialect = iota
	dialectChat
)

// ProcessChatMessages rewrites Chat Completions messages: image_url parts
// (for a model without vision; PDFs sent as images always) and file parts
// become text parts.
func ProcessChatMessages(ctx context.Context, messages []any, mc Context) []any {
	return rewriteOpenAI(ctx, messages, dialectChat, mc)
}

// ProcessResponsesInput rewrites Responses input items' input_image and
// input_file parts: message parts become input_text, parts in a tool's
// output become AttachmentTextType.
func ProcessResponsesInput(ctx context.Context, items []any, mc Context) []any {
	return rewriteOpenAI(ctx, items, dialectResponses, mc)
}

// action is what an OpenAI part becomes: kept, a job's result (key set),
// or fixed text.
type action struct {
	keep bool
	key  string
	job  job
	text string
}

func rewriteOpenAI(ctx context.Context, items []any, d dialect, mc Context) []any {
	jobs := map[string]job{}
	mapParts(items, d, func(part jsonx.Object, _ bool) []any {
		if a := classify(part, mc); a.key != "" {
			jobs[a.key] = a.job
		}
		return nil
	})
	results := runAll(ctx, jobs, mc)
	return mapParts(items, d, func(part jsonx.Object, inOutput bool) []any {
		a := classify(part, mc)
		if a.keep {
			return nil
		}
		text := a.text
		if a.key != "" {
			text = results[a.key]
		}
		typ := "input_text"
		switch {
		case d == dialectChat:
			typ = "text"
		case inOutput:
			typ = AttachmentTextType
		}
		return []any{textBlock(typ, text)}
	})
}

// mapParts applies replace to the parts of every message / item (nil keeps
// the part). inOutput: the part is in a tool's output.
func mapParts(items []any, d dialect, replace func(jsonx.Object, bool) []any) []any {
	parts := func(list []any, inOutput bool) []any {
		out := make([]any, 0, len(list))
		for _, raw := range list {
			if p, ok := raw.(jsonx.Object); ok {
				if r := replace(p, inOutput); r != nil {
					out = append(out, r...)
					continue
				}
			}
			out = append(out, raw)
		}
		return out
	}
	out := make([]any, len(items))
	for i, raw := range items {
		item, ok := raw.(jsonx.Object)
		if !ok {
			out[i] = raw
			continue
		}
		ni := maps.Clone(item)
		if content, ok := item["content"].([]any); ok {
			ni["content"] = parts(content, d == dialectChat && jsonx.Str(item["role"]) == "tool")
		}
		if d == dialectResponses {
			if output, ok := item["output"].([]any); ok {
				ni["output"] = parts(output, true)
			} else if o, ok := item["output"].(jsonx.Object); ok {
				if inner, ok := o["content"].([]any); ok {
					no := maps.Clone(o)
					no["content"] = parts(inner, true)
					ni["output"] = no
				}
			}
		}
		out[i] = ni
	}
	return out
}

func classify(part jsonx.Object, mc Context) action {
	switch jsonx.Type(part) {
	case "input_image", "image_url":
		return imagePart(part, mc)
	case "input_file", "file":
		return filePart(part)
	}
	return action{keep: true}
}

func imagePart(part jsonx.Object, mc Context) action {
	raw := part["image_url"]
	if raw == nil {
		raw = part["url"]
	}
	u, ok := raw.(string)
	if !ok {
		u = jsonx.Str(jsonx.Get(raw, "url"))
	}
	if u == "" {
		return action{text: "[image omitted: it is attached by file id, which code-link cannot read]"}
	}
	if mt, data, ok := DataURL(u); ok {
		if mt == "application/pdf" {
			title := jsonx.Str(part["filename"])
			return action{key: "doc|" + title + "||application/pdf|" + sha(data),
				job: docJob(source{data: data, mediaType: mt}, title, "")}
		}
		if mc.ModelHasVision {
			return action{keep: true}
		}
		return action{key: "img|" + mt + "|" + sha(data), job: imageJob(source{data: data, mediaType: mt})}
	}
	if mc.ModelHasVision {
		return action{keep: true}
	}
	return action{key: "imgurl|" + u, job: imageJob(source{url: u})}
}

func filePart(part jsonx.Object) action {
	f := jsonx.Obj(part["file"])
	if part["file"] == nil {
		f = part
	}
	name, _ := firstStr(f["filename"], part["filename"])
	title := trimSpaces(name)
	if data, _ := firstStr(f["file_data"], part["file_data"]); data != "" {
		mt, b64, ok := DataURL(data)
		if !ok {
			mt, b64 = guessMediaType(title, data), data
		}
		return action{key: "doc|" + title + "||" + mt + "|" + sha(b64),
			job: docJob(source{data: b64, mediaType: mt}, title, "")}
	}
	if u, _ := firstStr(f["file_url"], part["file_url"]); u != "" {
		return action{key: "docurl|" + title + "||" + u, job: docJob(source{url: u}, title, "")}
	}
	shown := title
	if shown == "" {
		shown = "unnamed file"
	}
	return action{text: "[attached file \"" + shown + "\" could not be read: only inline file_data is supported]"}
}

// DataURL splits a "data:<type>;base64,<data>" URL into its media type
// (lowercased; application/octet-stream when absent) and base64 data; ok
// false when s is not a base64 data URL.
func DataURL(s string) (mediaType, data string, ok bool) {
	rest, isData := strings.CutPrefix(s, "data:")
	if !isData {
		return "", "", false
	}
	head, data, hasComma := strings.Cut(rest, ",")
	if !hasComma {
		return "", "", false
	}
	var fields []string
	for f := range strings.SplitSeq(head, ";") {
		if f != "" {
			fields = append(fields, trimSpaces(f))
		}
	}
	isB64 := false
	for _, f := range fields {
		if strings.EqualFold(f, "base64") {
			isB64 = true
		}
	}
	if !isB64 {
		return "", "", false
	}
	mediaType = strings.ToLower(fields[0])
	if mediaType == "" || mediaType == "base64" {
		mediaType = "application/octet-stream"
	}
	return mediaType, data, true
}

// guessMediaType is the media type of inline file data without a data URL
// header: PDF by its magic, else by the file name's extension.
func guessMediaType(name, b64 string) string {
	if strings.HasPrefix(b64, "JVBERi") { // "%PDF-" in base64
		return "application/pdf"
	}
	ext := ""
	if parts := strings.FieldsFunc(name, func(r rune) bool { return r == '.' }); len(parts) > 0 {
		ext = strings.ToLower(parts[len(parts)-1])
	}
	switch ext {
	case "pdf":
		return "application/pdf"
	case "txt", "md", "log":
		return "text/plain"
	case "csv":
		return "text/csv"
	case "json":
		return "text/json"
	case "html", "htm":
		return "text/html"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "tif", "tiff":
		return "image/tiff"
	}
	return "application/octet-stream"
}
