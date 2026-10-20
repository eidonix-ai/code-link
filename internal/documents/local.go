package documents

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"codelink/internal/config"
)

// extracted is what local extraction read.
type extracted struct {
	text     string // pages joined by blank lines (empty pages dropped)
	pages    int
	ocrPages int
	via      string
}

// localError is why a document could not be read locally.
type localError struct{ why string }

func (e localError) Error() string { return e.why }

// minTextChars: a PDF page with fewer characters than this is OCR'd.
const minTextChars = 20

// localExtractor reads the PDF text layer per page; with OCR on, pages
// with fewer than minTextChars characters, and images, go to the
// platform's OCR.
type localExtractor struct {
	ocrOn   bool
	ocr     OCR
	textCap int
}

func (x localExtractor) extract(ctx context.Context, data []byte, mediaType string) (extracted, error) {
	switch {
	case mediaType == "application/pdf":
		return x.extractPDF(ctx, data)
	case strings.HasPrefix(mediaType, "image/"):
		return x.extractImage(ctx, data, mediaType)
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json":
		if !utf8.Valid(data) {
			return extracted{}, localError{"not valid UTF-8 text"}
		}
		return extracted{text: string(data), pages: 1, via: "text"}, nil
	}
	return extracted{}, localError{"unsupported document type " + mediaType}
}

func (x localExtractor) extractImage(ctx context.Context, data []byte, mediaType string) (extracted, error) {
	if !x.ocrOn {
		return extracted{}, localError{"images need OCR (PaddleOCR, or OCR turned on)"}
	}
	text, err := x.ocr.Image(ctx, data, mediaType)
	if errors.Is(err, ErrOCRUnavailable) {
		return extracted{}, localError{"images need OCR (PaddleOCR; this platform has no OCR yet)"}
	}
	if err != nil {
		return extracted{}, localError{"not a readable image"}
	}
	if text = strings.TrimSpace(text); text == "" {
		return extracted{}, localError{"no text found in the image"}
	}
	return extracted{text: text, pages: 1, ocrPages: 1, via: "ocr"}, nil
}

func (x localExtractor) extractPDF(ctx context.Context, data []byte) (extracted, error) {
	doc, err := openPDF(data)
	if err != nil {
		return extracted{}, localError{err.Error()}
	}
	var texts []string
	ocrPages, size := 0, 0
	ocrOn, ocrMissing := x.ocrOn, false
	for i := range doc.pages {
		if ctx.Err() != nil {
			break
		}
		text := strings.TrimSpace(doc.pageText(i))
		if ocrOn && utf8.RuneCountInString(text) < minTextChars {
			ocr, err := x.ocr.PDFPage(ctx, data, i)
			switch {
			case errors.Is(err, ErrOCRUnavailable):
				ocrOn, ocrMissing = false, true // don't ask again for every page
			case err == nil:
				if ocr = strings.TrimSpace(ocr); utf8.RuneCountInString(ocr) > utf8.RuneCountInString(text) {
					text = ocr
					ocrPages++
				}
			}
		}
		if text != "" {
			texts = append(texts, text)
			size += len(text)
		}
		if size > x.textCap {
			break // the cap will cut here; don't OCR the rest
		}
	}
	if len(texts) == 0 {
		switch {
		case ocrOn:
			return extracted{}, localError{"no text found"}
		case ocrMissing:
			return extracted{}, localError{"the PDF has no text layer (scanned) and this platform has no OCR yet; set up PaddleOCR"}
		}
		return extracted{}, localError{"the PDF has no text layer (scanned); turn on OCR or set up PaddleOCR"}
	}
	via := "pdf+ocr"
	switch ocrPages {
	case 0:
		via = "pdf"
	case len(texts):
		via = "ocr"
	}
	return extracted{text: strings.Join(texts, "\n\n"), pages: doc.pages, ocrPages: ocrPages, via: via}, nil
}

// localMode is the configured local extraction; anything unknown is OCR.
func localMode(l config.LocalExtraction) config.LocalExtraction {
	switch l {
	case config.LocalTextOnly, config.LocalOff:
		return l
	}
	return config.LocalOCR
}
