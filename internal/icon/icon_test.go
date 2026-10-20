package icon

import (
	"bytes"
	"image/png"
	"testing"
)

func TestIconsArePNGsOfTheirSize(t *testing.T) {
	for _, c := range []struct {
		b    []byte
		size int
	}{{Tray(44, Black, 1, 0.62, NoBadge, Black), 44}, {Tray(64, White, 0.45, 0.92, Error, Red), 64}, {App(128), 128}} {
		img, err := png.Decode(bytes.NewReader(c.b))
		if err != nil || img.Bounds().Dx() != c.size || img.Bounds().Dy() != c.size {
			t.Fatalf("%v %v", err, img.Bounds())
		}
	}
}

func TestTheBadgeShowsAndTheCornerIsOtherwiseClear(t *testing.T) {
	plain, _ := png.Decode(bytes.NewReader(Tray(64, Black, 1, 0.92, NoBadge, Red)))
	badged, _ := png.Decode(bytes.NewReader(Tray(64, Black, 1, 0.92, Error, Red)))
	if _, _, _, a := plain.At(51, 51).RGBA(); a != 0 {
		t.Error("no badge: the corner is clear")
	}
	if r, g, _, a := badged.At(51, 51).RGBA(); a == 0 || r <= g {
		t.Error("the badge is red")
	}
}
