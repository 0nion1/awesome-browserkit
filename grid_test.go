package browserkit

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

func TestGridAnnotationUsesCSSCoordinates(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 280, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 280; x++ {
			canvas.Set(x, y, color.RGBA{R: 30, G: 160, B: 160, A: 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, canvas); err != nil {
		t.Fatal(err)
	}
	output, err := annotateGrid(input.Bytes(), 580, 175, 280, 200)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(output))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 280 || decoded.Bounds().Dy() != 200 {
		t.Fatalf("网格改变了图片尺寸: %v", decoded.Bounds())
	}
	// 区域原点 580 之后的第一条 50px 网格线在图片 x=20。
	found := false
	for y := 0; y < 200; y++ {
		red, green, blue, _ := decoded.At(20, y).RGBA()
		if red>>8 > 30 && green>>8 < 160 || blue>>8 < 160 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("没有找到 600px 对应的网格线")
	}
	if path := os.Getenv("GRID_TEST_OUT"); path != "" {
		if err := os.WriteFile(path, output, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
