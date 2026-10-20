package media

import "testing"

func TestGuessMediaType(t *testing.T) {
	for _, c := range []struct{ name, b64, want string }{
		{"", "JVBERi0xLjQK", "application/pdf"}, // %PDF- magic wins over the name
		{"scan.png", "JVBERi0xLjQK", "application/pdf"},
		{"a.PDF", "AAAA", "application/pdf"},
		{"pdf", "AAAA", "application/pdf"}, // no dot: the whole name
		{"notes.txt", "", "text/plain"},
		{"README.md", "", "text/plain"},
		{"run.log", "", "text/plain"},
		{"t.csv", "", "text/csv"},
		{"d.json", "", "text/json"},
		{"p.htm", "", "text/html"},
		{"p.html", "", "text/html"},
		{"i.png", "", "image/png"},
		{"i.JPG", "", "image/jpeg"},
		{"i.jpeg", "", "image/jpeg"},
		{"i.gif", "", "image/gif"},
		{"i.webp", "", "image/webp"},
		{"i.tif", "", "image/tiff"},
		{"i.tiff", "", "image/tiff"},
		{"archive.zip", "", "application/octet-stream"},
		{"", "AAAA", "application/octet-stream"},
	} {
		if got := guessMediaType(c.name, c.b64); got != c.want {
			t.Errorf("guessMediaType(%q, %q) = %q, want %q", c.name, c.b64, got, c.want)
		}
	}
}

func TestDecodeLenient(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"aGkgdGhlcmU=", "hi there", true},
		{"aGkg\r\ndGhl\ncmU=", "hi there", true},
		{"aGkgdGhlcmU", "hi there", true},
		{"", "", true},
		{"@@@", "", false},
		{"A", "", false},
	} {
		got, ok := decodeLenient(c.in)
		if string(got) != c.want || ok != c.ok {
			t.Errorf("decodeLenient(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
