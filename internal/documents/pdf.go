package documents

import (
	"bytes"
	"errors"
	"math"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDF text layer, pure Go. github.com/ledongthuc/pdf (BSD, no
// dependencies; the maintained fork of rsc.io/pdf) parses the file: xref
// tables and streams, object streams, Flate/ASCII85, RC4/AES-128
// encryption with an empty user password, and font encodings including
// ToUnicode CMaps. Its own text extraction drops word spaces and line
// breaks, so the content streams are walked here with its Interpret: text
// is laid out from the text matrix (a new line when the baseline moves, a
// space when a gap opens on the line), and Form XObjects are followed.

var (
	errPDFUnreadable = errors.New("not a readable PDF")
	errPDFLocked     = errors.New("the PDF is password protected")
	errPDFEncryption = errors.New("the PDF's encryption is not supported")
)

type pdfDoc struct {
	r     *pdf.Reader
	pages int
	fonts map[string]*pdfFont
}

// openPDF parses data. The library panics on malformed input, so every
// call into it recovers.
func openPDF(data []byte) (doc *pdfDoc, err error) {
	defer func() {
		if recover() != nil {
			doc, err = nil, errPDFUnreadable
		}
	}()
	data = normalizePDF(data)
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		switch {
		case errors.Is(err, pdf.ErrInvalidPassword):
			return nil, errPDFLocked
		case strings.Contains(err.Error(), "encryption"):
			return nil, errPDFEncryption
		}
		return nil, errPDFUnreadable
	}
	return &pdfDoc{r: r, pages: r.NumPage(), fonts: map[string]*pdfFont{}}, nil
}

// normalizePDF works around the library's strict framing without moving
// any byte offsets: a PDF 2.0 header reads as 1.7, the header line may
// carry more than the version, and anything after the last %%EOF goes.
func normalizePDF(data []byte) []byte {
	if len(data) < 9 || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return data
	}
	var out []byte
	edit := func() {
		if out == nil {
			out = bytes.Clone(data)
		}
	}
	if data[5] == '2' {
		edit()
		copy(out[5:8], "1.7")
	}
	if data[8] != '\n' && data[8] != '\r' {
		edit()
		out[8] = '\n'
	}
	if i := bytes.LastIndex(data, []byte("%%EOF")); i >= 0 && len(bytes.TrimSpace(data[i+5:])) > 0 {
		edit()
		out = out[:i+5]
	}
	if out == nil {
		return data
	}
	return out
}

// pageText is page i's text (0-based); "" when the page has none or can't
// be read.
func (d *pdfDoc) pageText(i int) (text string) {
	w := &textWalker{doc: d}
	defer func() {
		if recover() != nil {
			text = tidy(w.out.String()) // what was read before the damage
		}
	}()
	p := d.r.Page(i + 1)
	if p.V.IsNull() {
		return ""
	}
	w.run(p.V.Key("Contents"), p.Resources(), 0)
	return tidy(w.out.String())
}

// tidy drops the trailing spaces some generators pad lines with.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// pdfFont is what text layout needs from a font.
type pdfFont struct {
	enc    pdf.TextEncoding
	type0  bool // two-byte codes, widths not read
	first  int
	widths []float64
}

func (d *pdfDoc) font(resources pdf.Value, name string) *pdfFont {
	v := resources.Key("Font").Key(name)
	key := name + "|" + v.Key("BaseFont").Name()
	if f, ok := d.fonts[key]; ok {
		return f
	}
	f := &pdfFont{type0: v.Key("Subtype").Name() == "Type0", first: int(v.Key("FirstChar").Int64())}
	func() {
		defer func() {
			if recover() != nil {
				f.enc = nil
			}
		}()
		f.enc = pdf.Font{V: v}.Encoder()
	}()
	if ws := v.Key("Widths"); ws.Len() > 0 {
		for i := range ws.Len() {
			f.widths = append(f.widths, ws.Index(i).Float64())
		}
	}
	d.fonts[key] = f
	return f
}

// matrix is a PDF matrix [a b c d e f].
type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// translate is T(tx, ty) × m.
func (m matrix) translate(tx, ty float64) matrix {
	m[4] += tx*m[0] + ty*m[2]
	m[5] += tx*m[1] + ty*m[3]
	return m
}

type textWalker struct {
	doc *pdfDoc
	out strings.Builder
	textState

	wrote          bool
	lastY, lastEnd float64
}

// textState is the graphics state text layout reads; a Form XObject runs
// with its own and the caller's comes back after.
type textState struct {
	font           *pdfFont
	size, leading  float64
	hscale         float64
	charSp, wordSp float64
	tm, tlm        matrix
}

const maxFormDepth = 8

func (w *textWalker) run(content, resources pdf.Value, depth int) {
	if content.IsNull() {
		return
	}
	w.hscale = 1
	w.tm, w.tlm = identity, identity
	pdf.Interpret(content, func(stk *pdf.Stack, op string) {
		n := stk.Len()
		args := make([]pdf.Value, n)
		for i := n - 1; i >= 0; i-- {
			args[i] = stk.Pop()
		}
		num := func(i int) float64 {
			if i < len(args) {
				return args[i].Float64()
			}
			return 0
		}
		switch op {
		case "BT":
			w.tm, w.tlm = identity, identity
		case "Tf":
			if len(args) == 2 {
				w.font = w.doc.font(resources, args[0].Name())
				w.size = num(1)
			}
		case "TL":
			w.leading = num(0)
		case "Tz":
			w.hscale = num(0) / 100
		case "Tc":
			w.charSp = num(0)
		case "Tw":
			w.wordSp = num(0)
		case "Td":
			w.moveLine(num(0), num(1))
		case "TD":
			w.leading = -num(1)
			w.moveLine(num(0), num(1))
		case "Tm":
			if len(args) == 6 {
				w.tm = matrix{num(0), num(1), num(2), num(3), num(4), num(5)}
				w.tlm = w.tm
			}
		case "T*":
			w.moveLine(0, -w.leading)
		case "Tj":
			if len(args) == 1 {
				w.show(args[0].RawString())
			}
		case "'":
			if len(args) == 1 {
				w.moveLine(0, -w.leading)
				w.show(args[0].RawString())
			}
		case "\"":
			if len(args) == 3 {
				w.wordSp, w.charSp = num(0), num(1)
				w.moveLine(0, -w.leading)
				w.show(args[2].RawString())
			}
		case "TJ":
			if len(args) != 1 {
				return
			}
			a := args[0]
			for i := range a.Len() {
				x := a.Index(i)
				if x.Kind() == pdf.String {
					w.show(x.RawString())
				} else {
					w.tm = w.tm.translate(-x.Float64()/1000*w.size*w.hscale, 0)
				}
			}
		case "Do":
			if len(args) == 1 && depth < maxFormDepth {
				x := resources.Key("XObject").Key(args[0].Name())
				if x.Key("Subtype").Name() == "Form" {
					res := x.Key("Resources")
					if res.IsNull() {
						res = resources
					}
					saved := w.textState
					func() {
						defer func() { recover() }() // an unreadable form loses only its own text
						w.run(x, res, depth+1)
					}()
					w.textState = saved
				}
			}
		}
	})
}

func (w *textWalker) moveLine(tx, ty float64) {
	w.tlm = w.tlm.translate(tx, ty)
	w.tm = w.tlm
}

// show writes a string drawn at the current text position: on a new line
// when the baseline moved, after a space when a gap opened.
func (w *textWalker) show(raw string) {
	if raw == "" {
		return
	}
	text := raw
	if w.font != nil && w.font.enc != nil {
		text = w.font.enc.Decode(raw)
	}
	x, y := w.tm[4], w.tm[5]
	em := math.Abs(w.size) * math.Hypot(w.tm[2], w.tm[3])
	if em == 0 {
		em = 1
	}
	if w.wrote {
		gap := x - w.lastEnd
		switch {
		case math.Abs(y-w.lastY) > em*0.6:
			w.newline()
		case gap > em*0.15 || gap < -em*2:
			w.space()
		}
	}
	for _, r := range text {
		if r == 0 {
			continue
		}
		w.out.WriteRune(r)
	}
	if strings.TrimSpace(text) != "" {
		w.wrote = true
	}
	w.tm = w.tm.translate(w.advance(raw), 0)
	w.lastY, w.lastEnd = y, w.tm[4]
}

// advance is the text-space width of a drawn string: glyph widths from the
// font where it lists them, else half an em per glyph.
func (w *textWalker) advance(raw string) float64 {
	glyphs, total, spaces := len(raw), 0.0, strings.Count(raw, " ")
	if w.font != nil && w.font.type0 {
		glyphs = len(raw) / 2
		total = float64(glyphs) * 500
		spaces = 0
	} else {
		for i := 0; i < len(raw); i++ {
			g := 500.0
			if w.font != nil {
				if j := int(raw[i]) - w.font.first; j >= 0 && j < len(w.font.widths) && w.font.widths[j] > 0 {
					g = w.font.widths[j]
				}
			}
			total += g
		}
	}
	return (total/1000*w.size + float64(glyphs)*w.charSp + float64(spaces)*w.wordSp) * w.hscale
}

func (w *textWalker) newline() {
	s := w.out.String()
	if s != "" && !strings.HasSuffix(s, "\n") {
		w.out.WriteByte('\n')
	}
}

func (w *textWalker) space() {
	s := w.out.String()
	if s != "" && !strings.HasSuffix(s, " ") && !strings.HasSuffix(s, "\n") {
		w.out.WriteByte(' ')
	}
}
