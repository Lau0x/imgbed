// fixtures 为 e2e.sh 生成测试文件，并提供 probe 子命令检查处理后的图片
//
//	fixtures <输出目录>   生成测试文件
//	fixtures probe <文件>  输出 "宽x高 格式 非白像素数"
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"

	"github.com/gen2brain/webp"
)

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

// photo 生成带渐变和噪点的图片，模拟照片，用来检验压缩效果
func photo(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			n := uint8(seed >> 27)
			img.SetNRGBA(x, y, color.NRGBA{uint8(x*255/w) ^ n, uint8(y*255/h) + n, 128 + n, 255})
		}
	}
	return img
}

func encJPEG(img image.Image, q int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: q}); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

func encPNG(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

// withOrientation 在 JPEG 的 SOI 之后插入只含 Orientation 标签的 EXIF 段
func withOrientation(j []byte, o uint16) []byte {
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, byte(o >> 8), byte(o), 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{}, j[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, j[2:]...)
}

func probe(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	var img image.Image
	format := ""
	if len(b) > 12 && string(b[8:12]) == "WEBP" {
		img, err = webp.Decode(bytes.NewReader(b))
		format = "webp"
	} else {
		img, format, err = image.Decode(bytes.NewReader(b))
	}
	if err != nil {
		log.Fatal(err)
	}
	r := img.Bounds()
	nonWhite := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			if cr < 0xF000 || cg < 0xF000 || cb < 0xF000 {
				nonWhite++
			}
		}
	}
	fmt.Printf("%dx%d %s %d\n", r.Dx(), r.Dy(), format, nonWhite)
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "probe" {
		probe(os.Args[2])
		return
	}
	if len(os.Args) != 2 {
		log.Fatal("用法：fixtures <输出目录> | fixtures probe <文件>")
	}
	dir := os.Args[1]
	small := solid(8, 8, color.NRGBA{255, 255, 255, 255})
	small.Set(1, 1, color.NRGBA{255, 0, 0, 255})

	write(filepath.Join(dir, "fixtures/a.png"), encPNG(small))
	write(filepath.Join(dir, "fixtures/white.png"), encPNG(solid(400, 300, color.NRGBA{255, 255, 255, 255})))
	write(filepath.Join(dir, "fixtures/photo.jpg"), encJPEG(photo(800, 600), 100))
	write(filepath.Join(dir, "fixtures/rot6.jpg"), withOrientation(encJPEG(photo(160, 80), 95), 6))
	write(filepath.Join(dir, "fixtures/shell.jpg"), []byte("<?php system($_GET[1]); ?>"))
	write(filepath.Join(dir, "fixtures/fake.jpg"), []byte("\xff\xd8\xff<?php echo 1; ?>"))
	write(filepath.Join(dir, "fixtures/big.png"), make([]byte, 12<<20))
	write(filepath.Join(dir, "fixtures/font.ttf"), []byte("not a font"))

	// 模拟一个 EasyImage 站点：旧图片、缩略图缓存和一个残留的后门
	write(filepath.Join(dir, "site/i/2024/05/03/old123.jpg"), encJPEG(small, 90))
	write(filepath.Join(dir, "site/i/cache/2024_05_03_old123.png"), encPNG(small))
	write(filepath.Join(dir, "site/i/2024/05/03/shell.php"), []byte("<?php eval($_POST[x]);"))

	// EasyImage 早期版本的 /images/ 目录
	write(filepath.Join(dir, "site/images/2021/04/06/legacy.th.jpg"), encJPEG(small, 90))
	write(filepath.Join(dir, "site/images/2021/04/06/evil.php"), []byte("<?php phpinfo();"))
}
