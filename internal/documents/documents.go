// Package documents turns documents into text for the model (docs/DESIGN.md
// §4.4): PaddleOCR first when configured and reachable, else the local
// extraction config.Documents.Local chooses — "ocr" (the PDF text layer,
// and the platform's OCR for pages without one), "text" (text layer only)
// or "off". Results are cached by SHA-256 so resent history isn't read
// again.
//
// Port of the Swift Features/Documents (DocumentService, PaddleOCRClient,
// LocalExtractor). PDFs are read in pure Go; see pdf.go.
package documents

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/lru"
)

// DefaultTextCap is the most text one document gives (UTF-8 bytes).
const DefaultTextCap = 600 * 1024

const (
	cacheEntries = 32
	cacheBytes   = 32 << 20
)

// Service is a hooks.DocumentReader. Safe for concurrent use.
type Service struct {
	local   config.LocalExtraction
	paddle  *paddleClient
	ocr     OCR
	textCap int
	cache   *lru.Cache[hooks.DocumentOutput]
}

var _ hooks.DocumentReader = (*Service)(nil)

// New reads documents per cfg; client (nil = http.DefaultClient) reaches
// PaddleOCR.
func New(cfg config.Documents, client *http.Client) *Service {
	s := &Service{local: localMode(cfg.Local), ocr: platformOCR(), textCap: DefaultTextCap,
		cache: lru.New[hooks.DocumentOutput](cacheEntries, cacheBytes)}
	if e := strings.TrimSpace(cfg.PaddleOCR); e != "" {
		s.paddle = newPaddle(e, client)
	}
	return s
}

// Read gives the document's text, or an Error saying why it could not.
func (s *Service) Read(ctx context.Context, doc hooks.DocumentInput) hooks.DocumentOutput {
	key := lru.SHA256(doc.Data)
	if hit, ok := s.cache.Get(key); ok {
		return hit
	}
	out := s.extract(ctx, doc)
	if out.Error == "" && ctx.Err() == nil {
		s.cache.Put(key, out, len(out.Text))
	}
	return out
}

func (s *Service) extract(ctx context.Context, doc hooks.DocumentInput) hooks.DocumentOutput {
	note := ""
	if s.paddle != nil {
		if s.paddle.reachable(ctx) {
			pages, err := s.paddle.read(ctx, doc.Data, doc.MediaType)
			if err == nil {
				var nonEmpty []string
				for _, p := range pages {
					if p != "" {
						nonEmpty = append(nonEmpty, p)
					}
				}
				return hooks.DocumentOutput{Text: capped(strings.Join(nonEmpty, "\n\n"), s.textCap),
					Pages: len(pages), OCRPages: len(pages), Via: "paddleocr"}
			}
			if !errors.Is(err, errPaddleUnsupported) { // not a PDF or image: the local path decides
				note = "PaddleOCR failed (" + err.Error() + ")"
			}
		} else {
			note = "PaddleOCR unreachable"
		}
	}
	return s.readLocal(ctx, doc, note)
}

func (s *Service) readLocal(ctx context.Context, doc hooks.DocumentInput, note string) hooks.DocumentOutput {
	if s.local == config.LocalOff {
		return hooks.DocumentOutput{Error: join(note, "document reading is turned off"), Via: "off"}
	}
	ocrOn := s.local == config.LocalOCR
	x := localExtractor{ocrOn: ocrOn, ocr: s.ocr, textCap: s.textCap}
	r, err := x.extract(ctx, doc.Data, doc.MediaType)
	if err != nil {
		via := "pdf"
		if ocrOn {
			via = "ocr"
		}
		return hooks.DocumentOutput{Error: join(note, err.Error()), Via: via}
	}
	return hooks.DocumentOutput{Text: capped(r.text, s.textCap), Pages: r.pages, OCRPages: r.ocrPages, Via: r.via}
}

func join(note, why string) string {
	if note == "" {
		return why
	}
	return note + "; " + why
}

// capped cuts text to limit UTF-8 bytes on a character boundary and says so.
func capped(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "\n\n[document truncated at " + strconv.Itoa(limit/1024) + " KB]"
}
