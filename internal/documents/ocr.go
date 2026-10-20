package documents

import (
	"context"
	"errors"
)

// OCR reads text the platform can see: a PDF page it renders itself, or
// an image. macOS (Vision) and Windows (Windows.Media.Ocr) implementations
// come later; until then platformOCR is unavailable and pages without a
// text layer are reported as such.
type OCR interface {
	// PDFPage reads page (0-based) of a PDF.
	PDFPage(ctx context.Context, pdf []byte, page int) (string, error)
	// Image reads an image (PNG, JPEG, …).
	Image(ctx context.Context, data []byte, mediaType string) (string, error)
}

// ErrOCRUnavailable: the platform has no OCR.
var ErrOCRUnavailable = errors.New("OCR is not available on this platform")

type unavailableOCR struct{}

func (unavailableOCR) PDFPage(context.Context, []byte, int) (string, error) {
	return "", ErrOCRUnavailable
}

func (unavailableOCR) Image(context.Context, []byte, string) (string, error) {
	return "", ErrOCRUnavailable
}

// platformOCR is this platform's OCR.
func platformOCR() OCR { return unavailableOCR{} }
