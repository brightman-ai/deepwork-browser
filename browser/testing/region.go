package testing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
)

// regionJS is the JS expression used to collect data-region elements and their bounding rects.
const regionJS = `JSON.stringify(Array.prototype.slice.call(document.querySelectorAll('[data-region]')).map(function(e){var r=e.getBoundingClientRect();return {id:e.getAttribute('data-region'),x:Math.round(r.x),y:Math.round(r.y),width:Math.round(r.width),height:Math.round(r.height)};}))`

// CollectRegionsViaEval collects page regions marked with data-region attributes
// by running JS via the provided evalFn. evalFn should call EvalJS(ctx, expr, &result).
// Elements with zero width/height are included (kept for completeness).
func CollectRegionsViaEval(evalFn func(expr string, result interface{}) error) ([]RegionSnap, error) {
	var jsonStr string
	if err := evalFn(regionJS, &jsonStr); err != nil {
		return nil, fmt.Errorf("collect regions: eval JS: %w", err)
	}

	type rawRegion struct {
		ID     string `json:"id"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	var raw []rawRegion
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("collect regions: parse JSON: %w", err)
	}

	regions := make([]RegionSnap, 0, len(raw))
	for _, r := range raw {
		regions = append(regions, RegionSnap{
			ID:   r.ID,
			Rect: Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height},
		})
	}
	return regions, nil
}

// CropRegion 从全屏截图中裁剪指定矩形区域
func CropRegion(fullScreenshot []byte, rect Rect) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(fullScreenshot))
	if err != nil {
		return nil, fmt.Errorf("region: decode screenshot: %w", err)
	}

	bounds := img.Bounds()

	// Clamp rect to image bounds
	x0 := clamp(rect.X, bounds.Min.X, bounds.Max.X)
	y0 := clamp(rect.Y, bounds.Min.Y, bounds.Max.Y)
	x1 := clamp(rect.X+rect.Width, bounds.Min.X, bounds.Max.X)
	y1 := clamp(rect.Y+rect.Height, bounds.Min.Y, bounds.Max.Y)

	cropRect := image.Rect(x0, y0, x1, y1)

	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}

	si, ok := img.(subImager)
	if !ok {
		return nil, fmt.Errorf("region: image type %T does not support SubImage", img)
	}

	cropped := si.SubImage(cropRect)

	var buf bytes.Buffer
	if err := png.Encode(&buf, cropped); err != nil {
		return nil, fmt.Errorf("region: encode cropped image: %w", err)
	}

	return buf.Bytes(), nil
}

// clamp returns v clamped to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
