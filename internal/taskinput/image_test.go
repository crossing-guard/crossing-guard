package taskinput

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func TestOrientImageAppliesAllEXIFTransforms(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	colors := []color.NRGBA{{R: 1, A: 255}, {R: 2, A: 255}, {R: 3, A: 255}, {R: 4, A: 255}, {R: 5, A: 255}, {R: 6, A: 255}}
	for index, value := range colors {
		source.SetNRGBA(index%2, index/2, value)
	}
	wants := map[int][]byte{
		1: {1, 2, 3, 4, 5, 6}, 2: {2, 1, 4, 3, 6, 5}, 3: {6, 5, 4, 3, 2, 1}, 4: {5, 6, 3, 4, 1, 2},
		5: {1, 3, 5, 2, 4, 6}, 6: {5, 3, 1, 6, 4, 2}, 7: {6, 4, 2, 5, 3, 1}, 8: {2, 4, 6, 1, 3, 5},
	}
	for orientation, want := range wants {
		got := orientImage(source, orientation)
		var red []byte
		for y := 0; y < got.Bounds().Dy(); y++ {
			for x := 0; x < got.Bounds().Dx(); x++ {
				red = append(red, got.NRGBAAt(x, y).R)
			}
		}
		if !bytes.Equal(red, want) {
			t.Fatalf("orientation %d: got %v want %v", orientation, red, want)
		}
	}
}

func TestImagePolicyDetectsAnimationAndMetadata(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black})
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{0, 0}}); err != nil {
		t.Fatal(err)
	}
	if !animatedImage(animated.Bytes(), "image/gif") {
		t.Fatal("animated GIF was not detected")
	}
	var static bytes.Buffer
	if err := png.Encode(&static, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	withICC := append(append([]byte{}, static.Bytes()[:12]...), append([]byte("iCCP"), static.Bytes()[12:]...)...)
	if !hasICCProfile(withICC, "image/png") {
		t.Fatal("PNG ICC marker was not detected")
	}
}
