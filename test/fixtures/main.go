// fixtures 为 e2e.sh 生成测试文件：真实图片、伪装成图片的 PHP、超大文件
package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func encode(path string, fn func(f *os.File, img image.Image) error) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := fn(f, img); err != nil {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("用法：fixtures <输出目录>")
	}
	dir := os.Args[1]
	pngFn := func(f *os.File, img image.Image) error { return png.Encode(f, img) }
	jpgFn := func(f *os.File, img image.Image) error { return jpeg.Encode(f, img, nil) }

	encode(filepath.Join(dir, "fixtures/a.png"), pngFn)
	write(filepath.Join(dir, "fixtures/shell.jpg"), []byte("<?php system($_GET[1]); ?>"))
	write(filepath.Join(dir, "fixtures/fake.jpg"), []byte("\xff\xd8\xff<?php echo 1; ?>"))
	write(filepath.Join(dir, "fixtures/big.png"), make([]byte, 12<<20))

	// 模拟一个 EasyImage 站点：旧图片、缩略图缓存和一个残留的后门
	encode(filepath.Join(dir, "site/i/2024/05/03/old123.jpg"), jpgFn)
	encode(filepath.Join(dir, "site/i/cache/2024_05_03_old123.png"), pngFn)
	write(filepath.Join(dir, "site/i/2024/05/03/shell.php"), []byte("<?php eval($_POST[x]);"))
}
