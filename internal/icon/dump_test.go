//go:build dumpicons

// Diagnostic: renders the meter icon at several fill levels to PNG files so they
// can be eyeballed. Writes one montage (all levels in a strip, scaled ×8 for
// visibility) plus the native-size individual icons. Excluded from normal runs.
//
//	ICON_DUMP_DIR=/tmp go test -tags dumpicons -run TestDump ./internal/icon
package icon

import (
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// nearest-neighbour upscale, so the 44px icons are legible when opened.
func scale(src *image.NRGBA, n int) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx()*n, b.Dy()*n))
	for y := 0; y < b.Dy()*n; y++ {
		for x := 0; x < b.Dx()*n; x++ {
			dst.Set(x, y, src.At(b.Min.X+x/n, b.Min.Y+y/n))
		}
	}
	return dst
}

func TestDump(t *testing.T) {
	dir := os.Getenv("ICON_DUMP_DIR")
	if dir == "" {
		dir = "."
	}

	// (session %, weekly %) pairs across the range, incl. the real 7/23 reading.
	pairs := [][2]float64{{0, 0}, {7, 23}, {30, 55}, {60, 80}, {88, 42}, {100, 100}}

	strip := image.NewNRGBA(image.Rect(0, 0, size*len(pairs), size))
	for i, p := range pairs {
		bi := barsImage(p[0], p[1])
		draw.Draw(strip, image.Rect(i*size, 0, (i+1)*size, size), bi, image.Point{}, draw.Src)

		name := fmt.Sprintf("bars-%03d-%03d.png", int(p[0]), int(p[1]))
		writePNG(t, filepath.Join(dir, name), bi)
	}
	writePNG(t, filepath.Join(dir, "bars-montage@8x.png"), scale(strip, 8))
	t.Logf("montage order (session,weekly): %v", pairs)
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
}
