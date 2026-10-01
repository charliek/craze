package attach

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"testing"

	"golang.org/x/image/webp"
)

// TestWebPFixtureDecodes is the fixture's own check: what webpSolid writes is
// a WebP x/image decodes to the colour it was asked for, or every WebP test
// below would be testing a broken file.
func TestWebPFixtureDecodes(t *testing.T) {
	c := color.NRGBA{0x12, 0x34, 0x56, 0xff}
	img, err := webp.Decode(bytes.NewReader(webpSolid(9, 13, c)))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 9 || b.Dy() != 13 {
		t.Fatalf("bounds %v", b)
	}
	if got := color.NRGBAModel.Convert(img.At(4, 6)); got != c {
		t.Fatalf("pixel %v, want %v", got, c)
	}
}

// TestProcessPassesThroughUntouched: a PNG, a WebP and a JPEG with no APP1
// that fit both limits come back as the very bytes handed in.
func TestProcessPassesThroughUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
		mime string
		w, h int
	}{
		{"png", encodePNG(t, gradient(64, 48)), MIMEPNG, 64, 48},
		{"webp", webpSolid(16, 24, color.NRGBA{1, 2, 3, 0xff}), MIMEWebP, 16, 24},
		{"jpeg", encodeJPEG(t, gradient(40, 30), 90), MIMEJPEG, 40, 30},
		{"png on the edge", encodePNG(t, gradient(MaxEdge, 8)), MIMEPNG, MaxEdge, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Process(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, tc.src) {
				t.Fatalf("the bytes changed: %d in, %d out", len(tc.src), len(got.Data))
			}
			if got.MIME != tc.mime || got.Width != tc.w || got.Height != tc.h || got.OrigWidth != tc.w || got.OrigHeight != tc.h {
				t.Fatalf("got %s %d×%d (from %d×%d), want %s %d×%d", got.MIME, got.Width, got.Height, got.OrigWidth, got.OrigHeight, tc.mime, tc.w, tc.h)
			}
			if got.Downscaled() {
				t.Fatal("a pass-through is not downscaled")
			}
		})
	}
}

// TestProcessStripsJPEGAPP1 is the pass-through's one exception: every APP1
// segment — EXIF with orientation 1, EXIF without one, XMP — is cut out, and
// nothing else changes: the result is byte for byte the JPEG the segments
// were inserted into.
func TestProcessStripsJPEGAPP1(t *testing.T) {
	plain := encodeJPEG(t, gradient(40, 30), 90)
	for _, tc := range []struct {
		name string
		segs [][]byte
	}{
		{"exif, orientation 1, little-endian", [][]byte{exifAPP1(1, false)}},
		{"exif, orientation 1, big-endian", [][]byte{exifAPP1(1, true)}},
		{"exif without an orientation", [][]byte{exifAPP1(0, false)}},
		{"xmp", [][]byte{xmpAPP1(100)}},
		{"exif and xmp", [][]byte{exifAPP1(1, false), xmpAPP1(100)}},
		{"an orientation out of range", [][]byte{exifAPP1(9, false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := afterSOI(plain, tc.segs...)
			got, err := Process(src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, plain) {
				t.Fatalf("got %d bytes, want the %d of the JPEG without its APP1", len(got.Data), len(plain))
			}
			if _, stripped, ok := jpegMeta(got.Data); !ok || len(stripped) != len(got.Data) {
				t.Fatal("an APP1 segment is left")
			}
			if got.MIME != MIMEJPEG || got.Width != 40 || got.Height != 30 {
				t.Fatalf("got %s %d×%d", got.MIME, got.Width, got.Height)
			}
		})
	}
}

// TestProcessChecksTheSizeAfterTheCut: a JPEG over the cap only because of
// its metadata passes through once the metadata is gone.
func TestProcessChecksTheSizeAfterTheCut(t *testing.T) {
	plain := encodeJPEG(t, gradient(40, 30), 90)
	src := afterSOI(plain, xmpAPP1(10_000))
	l := limits{maxBytes: len(plain), maxEdge: MaxEdge}
	got, err := l.process(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, plain) {
		t.Fatalf("got %s of %d bytes, want the stripped JPEG passed through", got.MIME, len(got.Data))
	}
}

// TestProcessAppliesOrientation: a JPEG whose EXIF orientation is not 1 is
// re-encoded upright. The expectations come from the EXIF definitions of the
// eight values ("the 0th row is the visual right-hand side…"), not from the
// transform's code: each quadrant of a 32×16 source must land where the
// definition puts it, and 5 to 8 swap the dimensions.
func TestProcessAppliesOrientation(t *testing.T) {
	src := encodeJPEG(t, quadrants(32, 16), 100)
	// Displayed top-left, top-right, bottom-left, bottom-right.
	for _, tc := range []struct {
		o    uint16
		want [4]color.RGBA
	}{
		{2, [4]color.RGBA{green, red, white, blue}},
		{3, [4]color.RGBA{white, blue, green, red}},
		{4, [4]color.RGBA{blue, white, red, green}},
		{5, [4]color.RGBA{red, blue, green, white}},
		{6, [4]color.RGBA{blue, red, white, green}},
		{7, [4]color.RGBA{white, green, blue, red}},
		{8, [4]color.RGBA{green, white, red, blue}},
	} {
		got, err := Process(afterSOI(src, exifAPP1(tc.o, tc.o%2 == 0)))
		if err != nil {
			t.Fatalf("orientation %d: %v", tc.o, err)
		}
		w, h := 32, 16
		if tc.o >= 5 {
			w, h = 16, 32
		}
		if got.Width != w || got.Height != h || got.OrigWidth != w || got.OrigHeight != h {
			t.Fatalf("orientation %d: %d×%d from %d×%d, want %d×%d", tc.o, got.Width, got.Height, got.OrigWidth, got.OrigHeight, w, h)
		}
		if got.Downscaled() {
			t.Fatalf("orientation %d: a rotation is not a downscale", tc.o)
		}
		img := decoded(t, got.Data)
		for i, at := range []image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}} {
			if c := img.At(at.X, at.Y); !near(c, tc.want[i], 40) {
				t.Errorf("orientation %d: quadrant %d is %v, want %v", tc.o, i, c, tc.want[i])
			}
		}
	}
}

// TestProcessGIFIsItsFirstFrame: a GIF is always re-encoded, as a PNG of its
// first frame only, drawn where that frame sits on the GIF's screen.
func TestProcessGIFIsItsFirstFrame(t *testing.T) {
	screen := image.Rect(0, 0, 16, 16)
	t.Run("full frames", func(t *testing.T) {
		src := encodeGIF(t, screen, solidFrame(screen, red), solidFrame(screen, blue))
		got, err := Process(src)
		if err != nil {
			t.Fatal(err)
		}
		if got.MIME != MIMEPNG || got.Width != 16 || got.Height != 16 {
			t.Fatalf("got %s %d×%d", got.MIME, got.Width, got.Height)
		}
		if c := decoded(t, got.Data).At(8, 8); !near(c, red, 0) {
			t.Fatalf("pixel %v, want the first frame's red", c)
		}
	})
	t.Run("a first frame smaller than the screen", func(t *testing.T) {
		src := encodeGIF(t, screen, solidFrame(image.Rect(4, 4, 12, 12), red), solidFrame(screen, blue))
		got, err := Process(src)
		if err != nil {
			t.Fatal(err)
		}
		if got.Width != 16 || got.Height != 16 {
			t.Fatalf("got %d×%d, want the screen", got.Width, got.Height)
		}
		img := decoded(t, got.Data)
		if c := img.At(8, 8); !near(c, red, 0) {
			t.Fatalf("inside the frame %v, want red", c)
		}
		if _, _, _, a := img.At(1, 1).RGBA(); a != 0 {
			t.Fatalf("outside the frame alpha %d, want transparent", a)
		}
	})
}

// TestProcessReencodesBMP: a BMP is never passed through, however small.
func TestProcessReencodesBMP(t *testing.T) {
	got, err := Process(encodeBMP(t, quadrants(16, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != MIMEPNG || got.Width != 16 || got.Height != 16 || got.Downscaled() {
		t.Fatalf("got %s %d×%d", got.MIME, got.Width, got.Height)
	}
	if c := decoded(t, got.Data).At(12, 12); !near(c, white, 0) {
		t.Fatalf("pixel %v, want white", c)
	}
}

// TestProcessDecodesWebP: a WebP over the edge is decoded and re-encoded (Go
// has no WebP encoder, so as PNG), and its colour survives.
func TestProcessDecodesWebP(t *testing.T) {
	c := color.NRGBA{0x20, 0x80, 0xc0, 0xff}
	got, err := Process(webpSolid(2400, 16, c))
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != MIMEPNG || got.Width != MaxEdge || got.Height != 13 || got.OrigWidth != 2400 || got.OrigHeight != 16 {
		t.Fatalf("got %s %d×%d from %d×%d", got.MIME, got.Width, got.Height, got.OrigWidth, got.OrigHeight)
	}
	if px := decoded(t, got.Data).At(1000, 6); !near(px, c, 2) {
		t.Fatalf("pixel %v, want %v", px, c)
	}
}

// TestProcessDownscalesToTheEdge: the real MaxEdge, on either axis, keeping
// the aspect ratio — and never taking the short edge under MinEdge, which the
// host would refuse.
func TestProcessDownscalesToTheEdge(t *testing.T) {
	for _, tc := range []struct {
		w, h, wantW, wantH int
	}{
		{2400, 120, MaxEdge, 100},
		{120, 2400, 100, MaxEdge},
		{2400, 8, MaxEdge, MinEdge},
	} {
		got, err := Process(encodePNG(t, gradient(tc.w, tc.h)))
		if err != nil {
			t.Fatalf("%d×%d: %v", tc.w, tc.h, err)
		}
		if got.MIME != MIMEPNG || got.Width != tc.wantW || got.Height != tc.wantH {
			t.Fatalf("%d×%d: got %s %d×%d, want %d×%d", tc.w, tc.h, got.MIME, got.Width, got.Height, tc.wantW, tc.wantH)
		}
		if !got.Downscaled() || got.OrigWidth != tc.w || got.OrigHeight != tc.h {
			t.Fatalf("%d×%d: the original is recorded as %d×%d", tc.w, tc.h, got.OrigWidth, got.OrigHeight)
		}
		if _, err := Validate(got.Data, got.MIME); err != nil {
			t.Fatalf("%d×%d: the host would refuse the output: %v", tc.w, tc.h, err)
		}
	}
}

// kernelBytes is the CatmullRom scaler's working buffer for an input sw×sh
// scaled to dw×dh: 32 bytes per pixel of output width × input height
// (x/image/draw's kernelScaler.makeTmpBuf).
func kernelBytes(sh, dw int) int64 { return 32 * int64(dw) * int64(sh) }

// TestShrinkStepsBoundTheScaler is the memory bound of Process (plan 033 C3r,
// r1 #4), checked on the sizes alone so no 50 MP buffer is made: every step
// halves only an axis at least twice its output, leaves it no smaller than the
// output, and after the last step every axis is under twice its output — so
// the kernel buffer for any source Probe lets through is at most 32 B × 2000 ×
// 3999. The r1 case, an 8 × 6,250,000 strip, asked 1.6 GB of the scaler
// without the halving; it now asks under 1 MB, and its biggest halving is
// half the strip.
func TestShrinkStepsBoundTheScaler(t *testing.T) {
	const bound = 32 * 2000 * 3999
	check := func(sw, sh int) (steps []image.Point, kernel int64) {
		t.Helper()
		dw, dh := scaledTo(sw, sh, MaxEdge)
		steps = shrinkSteps(sw, sh, dw, dh)
		w, h := sw, sh
		for _, p := range steps {
			if (p.X != w && (w < 2*dw || p.X != w/2)) || (p.Y != h && (h < 2*dh || p.Y != h/2)) || p.X < dw || p.Y < dh {
				t.Fatalf("%d×%d → %d×%d: step %d×%d → %v is not a halving of an axis twice its output", sw, sh, dw, dh, w, h, p)
			}
			w, h = p.X, p.Y
		}
		if w >= 2*dw || h >= 2*dh {
			t.Fatalf("%d×%d → %d×%d: the kernel's input is %d×%d", sw, sh, dw, dh, w, h)
		}
		if dw != w || dh != h {
			kernel = kernelBytes(h, dw)
		}
		if kernel > bound {
			t.Fatalf("%d×%d: the kernel buffer is %d bytes", sw, sh, kernel)
		}
		return steps, kernel
	}

	steps, kernel := check(8, 6_250_000)
	if last := steps[len(steps)-1]; last != image.Pt(8, 3051) || kernel != kernelBytes(3051, 8) || kernel > 1<<20 {
		t.Fatalf("8 × 6,250,000: steps end at %v, kernel %d bytes", last, kernel)
	}
	if first := steps[0]; first != image.Pt(8, 3_125_000) {
		t.Fatalf("8 × 6,250,000: the first halving is %v", first)
	}
	if _, kernel := check(3999, 3999); kernel != kernelBytes(3999, 2000) {
		t.Fatalf("3999²: kernel %d bytes, want the bound", kernel)
	}
	if steps, _ := check(2400, 1800); steps != nil {
		t.Fatalf("2400×1800 was halved: %v", steps)
	}
	// Every shape Probe lets through, coarsely: long edges up to the pixel
	// cap, aspect ratios from square to 8 px wide, both orientations.
	for long := MinEdge; long <= MaxSourcePixels/MinEdge; long = long*5/4 + 1 {
		for short := MinEdge; short <= long && long*short <= MaxSourcePixels; short = short*3/2 + 1 {
			check(long, short)
			check(short, long)
		}
	}
}

// TestFitHalvesBeforeTheKernel runs fit on images that need halving and
// watches what reaches the CatmullRom scaler (testKernelInput): never an
// input twice its output on an axis. The 8 × 100,000 strip is the r1 shape at
// a size a test can decode; its gradient survives the trip.
func TestFitHalvesBeforeTheKernel(t *testing.T) {
	var inputs []image.Rectangle
	testKernelInput = func(src image.Rectangle, w, h int) {
		if src.Dx() >= 2*w || src.Dy() >= 2*h {
			t.Errorf("CatmullRom got %v for %d×%d", src, w, h)
		}
		inputs = append(inputs, src)
	}
	t.Cleanup(func() { testKernelInput = nil })

	got, err := Process(encodePNG(t, gradient(8, 100_000)))
	if err != nil {
		t.Fatal(err)
	}
	// 8 × 100,000 halves five times to 8 × 3125, which CatmullRom takes to
	// 8 × 2000.
	if got.Width != MinEdge || got.Height != MaxEdge || len(inputs) != 1 || inputs[0] != image.Rect(0, 0, 8, 3125) {
		t.Fatalf("got %d×%d through %v", got.Width, got.Height, inputs)
	}
	out := decoded(t, got.Data)
	top, bottom := out.At(4, 10), out.At(4, got.Height-10)
	if near(top, bottom, 16) {
		t.Fatalf("the gradient flattened: %v at the top, %v at the bottom", top, bottom)
	}
}

// TestHalvingKeepsTheImage: an image halved twice before CatmullRom comes out
// the same as one CatmullRom took the whole way — to within a few levels a
// channel on a smooth ramp, and with every quadrant's colour where the
// quadrants meet nothing (the ladder's own quality, plan 033 §3.2, holds).
// A hard edge is where they may differ: the halving's box and the kernel's
// lobes place it a pixel apart.
func TestHalvingKeepsTheImage(t *testing.T) {
	l := limits{maxBytes: MaxBytes, maxEdge: 200}
	ramp := image.NewNRGBA(image.Rect(0, 0, 1600, 1200))
	for y := range 1200 {
		for x := range 1600 {
			ramp.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / 1599), uint8(y * 255 / 1199), uint8((x + y) * 255 / 2798), 0xff})
		}
	}
	halved, direct := l.fit(ramp), scale(ramp, 200, 150)
	if halved.Rect != image.Rect(0, 0, 200, 150) {
		t.Fatalf("ramp: fit to %v", halved.Rect)
	}
	worst := 0
	for i := range halved.Pix {
		d := int(halved.Pix[i]) - int(direct.Pix[i])
		worst = max(worst, d, -d)
	}
	if worst > 2 {
		t.Fatalf("ramp: halved and direct differ by %d levels", worst)
	}
	q := quadrants(1600, 1200)
	halved, direct = l.fit(q), scale(q, 200, 150)
	for _, p := range []image.Point{{50, 37}, {150, 37}, {50, 112}, {150, 112}, {2, 2}, {197, 147}} {
		if !near(halved.At(p.X, p.Y), direct.At(p.X, p.Y), 2) {
			t.Fatalf("quadrants: %v is %v halved, %v direct", p, halved.At(p.X, p.Y), direct.At(p.X, p.Y))
		}
	}
}

// TestProcessAlphaGoesToJPEGOnWhite: an image still over the cap as PNG
// becomes a JPEG, and what was transparent comes out white, not black.
func TestProcessAlphaGoesToJPEGOnWhite(t *testing.T) {
	img := noise(96, 96, 7)
	for y := range 96 {
		for x := range 48 {
			img.SetNRGBA(x, y, color.NRGBA{}) // the left half fully transparent
		}
	}
	l := limits{maxBytes: len(encodePNG(t, img)) - 1, maxEdge: MaxEdge}
	got, err := l.process(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != MIMEJPEG || len(got.Data) > l.maxBytes {
		t.Fatalf("got %s of %d bytes, want a JPEG within %d", got.MIME, len(got.Data), l.maxBytes)
	}
	out := decoded(t, got.Data)
	if c := out.At(got.Width/8, got.Height/2); !near(c, white, 8) {
		t.Fatalf("a transparent pixel came out %v, want white", c)
	}
}

// TestProcessLadder walks the step-down: a cap the second rung meets keeps
// the size, and a cap even the last rung misses shrinks the image by 0.75 —
// and however small the cap, the result converges under it, still decodes,
// and keeps its aspect ratio.
func TestProcessLadder(t *testing.T) {
	img := noise(200, 100, 11)
	src := encodePNG(t, img)
	flat := flatten(toRGBA(img))
	q85, q75, q45 := len(encodeJPEG(t, flat, 85)), len(encodeJPEG(t, flat, 75)), len(encodeJPEG(t, flat, 45))
	if q75 >= q85 || q45 >= q75 {
		t.Fatalf("setup: the rungs do not shrink: %d, %d, %d", q85, q75, q45)
	}
	t.Run("the second rung", func(t *testing.T) {
		got, err := limits{maxBytes: q75, maxEdge: MaxEdge}.process(src)
		if err != nil {
			t.Fatal(err)
		}
		if got.MIME != MIMEJPEG || got.Width != 200 || got.Height != 100 || len(got.Data) != q75 {
			t.Fatalf("got %s %d×%d of %d bytes, want the quality-75 JPEG of %d", got.MIME, got.Width, got.Height, len(got.Data), q75)
		}
	})
	t.Run("one shrink", func(t *testing.T) {
		got, err := limits{maxBytes: q45 - 1, maxEdge: MaxEdge}.process(src)
		if err != nil {
			t.Fatal(err)
		}
		if got.MIME != MIMEJPEG || got.Width != 150 || got.Height != 75 {
			t.Fatalf("got %s %d×%d, want a JPEG at 150×75", got.MIME, got.Width, got.Height)
		}
	})
	t.Run("converges", func(t *testing.T) {
		const capBytes = 2500
		got, err := limits{maxBytes: capBytes, maxEdge: MaxEdge}.process(src)
		if err != nil {
			t.Fatal(err)
		}
		if got.MIME != MIMEJPEG || len(got.Data) > capBytes {
			t.Fatalf("got %s of %d bytes, want a JPEG within %d", got.MIME, len(got.Data), capBytes)
		}
		if !got.Downscaled() || got.OrigWidth != 200 || got.OrigHeight != 100 {
			t.Fatalf("got %d×%d from %d×%d", got.Width, got.Height, got.OrigWidth, got.OrigHeight)
		}
		if r := float64(got.Width) / float64(got.Height); r < 1.9 || r > 2.1 {
			t.Fatalf("the aspect ratio drifted: %d×%d", got.Width, got.Height)
		}
		if b := decoded(t, got.Data).Bounds(); b.Dx() != got.Width || b.Dy() != got.Height {
			t.Fatalf("the output decodes to %v, not %d×%d", b, got.Width, got.Height)
		}
	})
}

func toRGBA(img image.Image) *image.RGBA {
	return limits{maxBytes: MaxBytes, maxEdge: MaxEdge}.fit(img)
}

// TestProcessRefuses is every refusal, each before anything costly: the
// oversized source by its length, the 50 MP one by a crafted header alone
// (no 50 MP buffer is ever made), the tiny one, and what is not an image.
func TestProcessRefuses(t *testing.T) {
	oversize := make([]byte, MaxSourceBytes+1)
	copy(oversize, encodePNG(t, gradient(16, 16)))
	truncated := encodePNG(t, gradient(64, 64))
	truncated = truncated[:len(truncated)/2]
	for _, tc := range []struct {
		name string
		src  []byte
		want error
	}{
		{"over 20 MiB", oversize, ErrSourceTooLarge},
		{"over 50 megapixels", pngHeader(10_000, 5_001, 0), ErrTooManyPixels},
		{"7 wide", encodePNG(t, gradient(7, 8)), ErrTooSmall},
		{"7 high", encodePNG(t, gradient(8, 7)), ErrTooSmall},
		{"text", []byte("hello, this is not an image"), ErrNotImage},
		{"empty", nil, ErrNotImage},
		{"a truncated png", truncated, ErrNotImage},
		{"a header with no image after it", pngHeader(16, 16, 32), ErrNotImage},
		{"tiff", []byte("II*\x00\x08\x00\x00\x00"), ErrNotImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Process(tc.src)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Process = %v, want %v", err, tc.want)
			}
			if got.Data != nil {
				t.Fatal("a refusal returned data")
			}
		})
	}
	// Exactly 50 MP is allowed by the header check: the refusal is "over".
	if _, err := Probe(bytes.NewReader(pngHeader(10_000, 5_000, 0))); err != nil {
		t.Fatalf("Probe of exactly 50 MP = %v", err)
	}
}

// TestValidate is the host's check of a stored file, from its bytes only.
func TestValidate(t *testing.T) {
	pngBytes := encodePNG(t, gradient(16, 16))
	jpgBytes := encodeJPEG(t, gradient(16, 16), 90)
	for _, tc := range []struct {
		name string
		b    []byte
		mime string
		want error
	}{
		{"png", pngBytes, MIMEPNG, nil},
		{"jpeg", jpgBytes, MIMEJPEG, nil},
		{"webp", webpSolid(8, 8, color.NRGBA{A: 0xff}), MIMEWebP, nil},
		{"a jpeg named png", jpgBytes, MIMEPNG, ErrWrongType},
		{"a png named jpeg", pngBytes, MIMEJPEG, ErrWrongType},
		{"a gif", encodeGIF(t, image.Rect(0, 0, 8, 8), solidFrame(image.Rect(0, 0, 8, 8), red)), mimeGIF, ErrWrongType},
		{"text named png", []byte("hello"), MIMEPNG, ErrWrongType},
		{"png magic, no header", []byte("\x89PNG\r\n\x1a\n"), MIMEPNG, ErrNotImage},
		{"a header claiming 2001 wide", pngHeader(2001, 10, 0), MIMEPNG, ErrOverEdge},
		{"a header claiming 7 high", pngHeader(10, 7, 0), MIMEPNG, ErrTooSmall},
		{"over the byte cap", pngHeader(16, 16, MaxBytes), MIMEPNG, ErrTooLarge},
		{"exactly the byte cap", pngHeader(16, 16, MaxBytes-len(pngHeader(16, 16, 0))), MIMEPNG, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(tc.b, tc.mime)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSniff(t *testing.T) {
	for _, tc := range []struct {
		b    string
		want string
	}{
		{"\x89PNG\r\n\x1a\nxx", MIMEPNG},
		{"\xff\xd8\xff\xe0", MIMEJPEG},
		{"GIF89a", mimeGIF},
		{"GIF87a", mimeGIF},
		{"RIFF\x00\x00\x00\x00WEBP", MIMEWebP},
		{"RIFF\x00\x00\x00\x00WAVE", ""},
		{"BM", mimeBMP},
		{"\x89PNG", ""},
		{"", ""},
	} {
		if got := Sniff([]byte(tc.b)); got != tc.want {
			t.Errorf("Sniff(%q) = %q, want %q", tc.b, got, tc.want)
		}
	}
}
