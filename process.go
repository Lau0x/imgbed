package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gen2brain/webp"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// 超过这个像素数的图片不做处理，原样保存，防止超大尺寸图片把内存撑爆
const maxProcessPixels = 40_000_000

// process 按后台设置压缩、转格式、加水印，返回处理后的数据和扩展名。
// 动图、超大图片、处理失败时都原样返回；没加水印且处理后反而变大时也保留原图。
func (a *app) process(data []byte, ext string, s Settings) ([]byte, string) {
	if !s.needsProcessing() || ext == "gif" || (ext == "webp" && animatedWebP(data)) {
		return data, ext
	}
	var cfg image.Config
	var err error
	if ext == "webp" {
		cfg, err = webp.DecodeConfig(bytes.NewReader(data))
	} else {
		cfg, _, err = image.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil || cfg.Width*cfg.Height > maxProcessPixels {
		return data, ext
	}

	a.procSem <- struct{}{}
	defer func() { <-a.procSem }()

	var src image.Image
	if ext == "webp" {
		src, err = webp.Decode(bytes.NewReader(data))
	} else {
		src, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		log.Printf("解码图片失败，按原图保存：%v", err)
		return data, ext
	}
	img := toNRGBA(src)
	if ext == "jpg" {
		img = orient(img, jpegOrientation(data))
	}

	watermarked := false
	if s.Watermark && s.WaterText != "" {
		watermarked = a.drawWatermark(img, s)
	}

	outExt := ext
	if s.Convert == "webp" {
		outExt = "webp"
	}
	quality := s.Quality
	if !s.Compress {
		quality = 92 // 只转格式或加水印时用接近无损的质量
	}
	var buf bytes.Buffer
	switch outExt {
	case "jpg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	case "png":
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img)
	case "webp":
		err = webp.Encode(&buf, img, webp.Options{Quality: quality, Method: 4})
	}
	if err != nil {
		log.Printf("编码图片失败，按原图保存：%v", err)
		return data, ext
	}
	if !watermarked && buf.Len() >= len(data) {
		return data, ext
	}
	return buf.Bytes(), outExt
}

// animatedWebP 检查 VP8X 扩展头里的动画标志位
func animatedWebP(b []byte) bool {
	return len(b) > 20 && string(b[12:16]) == "VP8X" && b[20]&0x02 != 0
}

func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	if n, ok := src.(*image.NRGBA); ok && b.Min == (image.Point{}) {
		return n
	}
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// jpegOrientation 读取 JPEG 中 EXIF 的 Orientation 标签，取不到时返回 1（正常方向）
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xD9 || marker == 0xDA {
			return 1
		}
		size := int(b[i+2])<<8 | int(b[i+3])
		if size < 2 || i+2+size > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient 按 EXIF 方向把像素转正，重新编码后不再依赖 EXIF
func orient(src *image.NRGBA, o int) *image.NRGBA {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			copy(dst.Pix[dy*dst.Stride+dx*4:dy*dst.Stride+dx*4+4], src.Pix[y*src.Stride+x*4:y*src.Stride+x*4+4])
		}
	}
	return dst
}

func parseHexColor(s string) (color.NRGBA, bool) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.NRGBA{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}, true
}

func (a *app) fontPath() string {
	return filepath.Join(a.cfg.configDir, "watermark-font")
}

// parseFont 支持 ttf / otf 以及 ttc 字体集（取第一个字体）
func parseFont(b []byte) (*opentype.Font, error) {
	if f, err := opentype.Parse(b); err == nil {
		return f, nil
	}
	c, err := opentype.ParseCollection(b)
	if err != nil {
		return nil, errors.New("无法识别的字体文件")
	}
	return c.Font(0)
}

// loadFont 读取后台上传的字体；没有上传时用内置的 Go 字体（只含西文字符）
func (a *app) loadFont() {
	f, custom := (*opentype.Font)(nil), false
	if b, err := os.ReadFile(a.fontPath()); err == nil {
		if parsed, err := parseFont(b); err == nil {
			f, custom = parsed, true
		} else {
			log.Printf("水印字体无法解析，改用内置字体：%v", err)
		}
	}
	if f == nil {
		f, _ = opentype.Parse(goregular.TTF)
	}
	a.fontMu.Lock()
	a.font, a.fontCustom = f, custom
	a.fontMu.Unlock()
}

func (a *app) currentFont() (*opentype.Font, bool) {
	a.fontMu.RLock()
	defer a.fontMu.RUnlock()
	return a.font, a.fontCustom
}

// missingGlyphs 返回当前字体里没有的字符，用来提示上传中文字体
func (a *app) missingGlyphs(text string) string {
	f, _ := a.currentFont()
	var buf sfnt.Buffer
	var missing []rune
	seen := map[rune]bool{}
	for _, r := range text {
		if r == ' ' || seen[r] {
			continue
		}
		if idx, err := f.GlyphIndex(&buf, r); err != nil || idx == 0 {
			missing = append(missing, r)
			seen[r] = true
		}
	}
	return string(missing)
}

// drawWatermark 在图片上画文字水印，带一层半透明阴影，深浅背景上都能看清
func (a *app) drawWatermark(img *image.NRGBA, s Settings) bool {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	short := w
	if h < short {
		short = h
	}
	if short < 100 {
		return false
	}
	f, _ := a.currentFont()
	size := float64(short) * float64(s.WaterSize) / 100
	if size < 12 {
		size = 12
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return false
	}
	defer face.Close()

	col, ok := parseHexColor(s.WaterColor)
	if !ok {
		col = color.NRGBA{255, 255, 255, 255}
	}
	col.A = uint8(s.WaterOpacity * 255 / 100)

	d := &font.Drawer{Dst: img, Face: face}
	tw := d.MeasureString(s.WaterText).Ceil()
	m := face.Metrics()
	asc, th := m.Ascent.Ceil(), m.Ascent.Ceil()+m.Descent.Ceil()
	margin := int(size * 0.8)
	pos := s.WaterPos
	if pos < 1 || pos > 9 {
		pos = 9
	}
	var x, y int
	switch (pos - 1) % 3 {
	case 0:
		x = margin
	case 1:
		x = (w - tw) / 2
	default:
		x = w - tw - margin
	}
	switch (pos - 1) / 3 {
	case 0:
		y = margin
	case 1:
		y = (h - th) / 2
	default:
		y = h - th - margin
	}
	off := int(size / 18)
	if off < 1 {
		off = 1
	}
	d.Src = image.NewUniform(color.NRGBA{0, 0, 0, col.A / 2})
	d.Dot = fixed.P(x+off, y+asc+off)
	d.DrawString(s.WaterText)
	d.Src = image.NewUniform(col)
	d.Dot = fixed.P(x, y+asc)
	d.DrawString(s.WaterText)
	return true
}
