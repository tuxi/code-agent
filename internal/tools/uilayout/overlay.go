package uilayout

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

const maxOverlayDimension = 2048

var overlayColors = map[string]color.RGBA{
	"observed":  {34, 153, 84, 255},
	"inferred":  {217, 139, 24, 255},
	"uncertain": {200, 55, 55, 255},
}

func renderOverlay(source image.Image, root layoutNode) ([]byte, int, int, error) {
	dst := fitOverlay(source)
	drawLayoutNodes(dst, root, true)
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, 0, 0, fmt.Errorf("encode overlay: %w", err)
	}
	return out.Bytes(), dst.Bounds().Dx(), dst.Bounds().Dy(), nil
}

func fitOverlay(source image.Image) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maxOverlayDimension && height <= maxOverlayDimension {
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(dst, dst.Bounds(), source, bounds.Min, draw.Src)
		return dst
	}
	scale := math.Min(float64(maxOverlayDimension)/float64(width), float64(maxOverlayDimension)/float64(height))
	newWidth := max(1, int(float64(width)*scale))
	newHeight := max(1, int(float64(height)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	for y := 0; y < newHeight; y++ {
		sy := bounds.Min.Y + y*height/newHeight
		for x := 0; x < newWidth; x++ {
			sx := bounds.Min.X + x*width/newWidth
			dst.Set(x, y, source.At(sx, sy))
		}
	}
	return dst
}

func drawLayoutNodes(dst *image.RGBA, node layoutNode, root bool) {
	if node.BBox != nil {
		width := dst.Bounds().Dx()
		height := dst.Bounds().Dy()
		x, y, w, h := node.BBox.pixelRect(width, height)
		if w > 0 && h > 0 {
			rect := image.Rect(x, y, x+w, y+h)
			thickness := max(2, min(width, height)/400)
			thickness = min(thickness, 8)
			if root {
				thickness = min(thickness+1, 10)
			}
			fill := overlayColor(node.Evidence)
			strokeRect(dst, rect, thickness, fill)
			labelScale := 1
			if width >= 400 {
				labelScale = 2
			}
			drawLabel(dst, x+thickness, y+thickness, node.ID, fill, labelScale)
		}
	}
	for _, child := range node.Children {
		drawLayoutNodes(dst, child, false)
	}
}

func overlayColor(evidence string) color.RGBA {
	if c, ok := overlayColors[evidence]; ok {
		return c
	}
	return color.RGBA{110, 110, 110, 255}
}
