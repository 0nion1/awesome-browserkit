package browserkit

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
)

const maxGridPixels = 16 << 20

var gridDigits = [10][7]string{
	{"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	{"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	{"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	{"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	{"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	{"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	{"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	{"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	{"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	{"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
}

// annotateGrid 在截图上叠加页面 CSS 坐标网格；坐标按 region 原点换算。
func annotateGrid(data []byte, originX, originY, regionWidth, regionHeight float64) ([]byte, error) {
	if regionWidth <= 0 || regionHeight <= 0 {
		return nil, errors.New("网格截图需要正数区域尺寸")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width*config.Height > maxGridPixels {
		return nil, errors.New("网格截图尺寸超过限制")
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	canvas := image.NewRGBA(decoded.Bounds())
	draw.Draw(canvas, canvas.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	width := float64(decoded.Bounds().Dx())
	height := float64(decoded.Bounds().Dy())
	scaleX := width / regionWidth
	scaleY := height / regionHeight
	line := color.RGBA{R: 255, G: 80, B: 80, A: 90}
	for cssX := math.Ceil(originX/50) * 50; cssX <= originX+regionWidth; cssX += 50 {
		x := int(math.Round((cssX - originX) * scaleX))
		if x >= 0 && x < int(width) {
			draw.Draw(canvas, image.Rect(x, 0, x+1, int(height)), &image.Uniform{C: line}, image.Point{}, draw.Over)
			if int(cssX)%100 == 0 {
				drawNumber(canvas, int(cssX), x+2, 2)
			}
		}
	}
	for cssY := math.Ceil(originY/50) * 50; cssY <= originY+regionHeight; cssY += 50 {
		y := int(math.Round((cssY - originY) * scaleY))
		if y >= 0 && y < int(height) {
			draw.Draw(canvas, image.Rect(0, y, int(width), y+1), &image.Uniform{C: line}, image.Point{}, draw.Over)
			if int(cssY)%100 == 0 {
				drawNumber(canvas, int(cssY), 2, y+2)
			}
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func drawNumber(canvas *image.RGBA, value, x, y int) {
	text := strconv.Itoa(value)
	for index, character := range text {
		if character < '0' || character > '9' {
			continue
		}
		pattern := gridDigits[character-'0']
		offset := index * 12
		for row, bits := range pattern {
			for column, bit := range bits {
				if bit != '1' {
					continue
				}
				px := x + offset + column*2
				py := y + row*2
				draw.Draw(canvas, image.Rect(px, py, px+2, py+2), &image.Uniform{C: color.RGBA{20, 20, 20, 230}}, image.Point{}, draw.Src)
			}
		}
	}
}
