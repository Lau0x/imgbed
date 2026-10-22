package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	errTooLarge = errors.New("文件超过大小限制")
	errNotImage = errors.New("仅支持 jpg / png / gif / webp 图片")
)

// uploadResult 与 EasyImage api/index.php 的返回格式保持一致，PicGo 等客户端无需改配置
type uploadResult struct {
	Result  string `json:"result"`
	Code    int    `json:"code"`
	URL     string `json:"url,omitempty"`
	SrcName string `json:"srcName,omitempty"`
	Thumb   string `json:"thumb,omitempty"`
	Del     string `json:"del,omitempty"`
	Message string `json:"message"`
}

func failed(code int, msg string) uploadResult {
	return uploadResult{Result: "failed", Code: code, Message: msg}
}

func writeJSON(w http.ResponseWriter, v uploadResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 让 del 链接里的 & 原样输出，和 EasyImage 一致
	enc.Encode(v)
}

// sniffImage 只看文件内容判断格式，不信任文件名和 Content-Type
func sniffImage(h []byte) string {
	switch {
	case bytes.HasPrefix(h, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg"
	case bytes.HasPrefix(h, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(h, []byte("GIF87a")), bytes.HasPrefix(h, []byte("GIF89a")):
		return "gif"
	case len(h) >= 16 && string(h[0:4]) == "RIFF" && string(h[8:12]) == "WEBP" && string(h[12:15]) == "VP8":
		return "webp"
	}
	return ""
}

func randomName() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 10)
	buf := make([]byte, 16)
	for len(out) < 10 {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if b < 252 && len(out) < 10 { // 252 = 36*7，避免取模偏差
				out = append(out, alphabet[b%36])
			}
		}
	}
	return string(out)
}

// saveImage 校验并保存图片，返回相对 /i/ 的路径，例如 2026/10/22/k88e7pabcd.jpg
func (a *app) saveImage(fh *multipart.FileHeader) (string, error) {
	if fh.Size > a.cfg.maxBytes {
		return "", errTooLarge
	}
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()

	head := make([]byte, 32)
	n, _ := io.ReadFull(src, head)
	ext := sniffImage(head[:n])
	if ext == "" {
		return "", errNotImage
	}
	if ext != "webp" {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if _, _, err := image.DecodeConfig(src); err != nil {
			return "", errNotImage
		}
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	day := time.Now().Format("2006/01/02")
	dir := filepath.Join(a.cfg.imagesDir, filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	written, err := io.Copy(tmp, io.LimitReader(src, a.cfg.maxBytes+1))
	if err == nil && written > a.cfg.maxBytes {
		err = errTooLarge
	}
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}

	for i := 0; i < 5; i++ {
		name := randomName() + "." + ext
		final := filepath.Join(dir, name)
		if _, err := os.Lstat(final); os.IsNotExist(err) {
			if err := os.Rename(tmp.Name(), final); err != nil {
				return "", err
			}
			return day + "/" + name, nil
		}
	}
	return "", errors.New("生成文件名失败")
}

func (a *app) receive(w http.ResponseWriter, r *http.Request, checkToken bool) {
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.maxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, failed(400, "请求无效或文件超过大小限制"))
		return
	}
	defer r.MultipartForm.RemoveAll()

	if checkToken {
		token := r.FormValue("token")
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if !a.validToken(strings.TrimSpace(token)) {
			writeJSON(w, failed(202, "Token 错误"))
			return
		}
	}

	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		writeJSON(w, failed(204, "没有选择上传的文件"))
		return
	}
	fh := files[0]
	rel, err := a.saveImage(fh)
	if err != nil {
		if errors.Is(err, errTooLarge) || errors.Is(err, errNotImage) {
			writeJSON(w, failed(400, err.Error()))
		} else {
			log.Printf("保存图片失败：%v", err)
			writeJSON(w, failed(500, "保存失败"))
		}
		return
	}
	log.Printf("上传 %s（来源 %s）", rel, a.clientIP(r))

	link := a.cfg.baseURL + "/i/" + rel
	writeJSON(w, uploadResult{
		Result:  "success",
		Code:    200,
		URL:     link,
		SrcName: strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)),
		Thumb:   link,
		Del:     a.cfg.baseURL + "/del?f=" + url.QueryEscape(rel) + "&s=" + a.sign("del|"+rel),
		Message: "success",
	})
}

// handleUpload 供网页上传使用，靠登录 Cookie 鉴权
func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.loggedIn(r) || !sameOrigin(r) {
		writeJSON(w, failed(401, "未登录或登录已过期"))
		return
	}
	a.receive(w, r, false)
}

// handleAPI 兼容 EasyImage 的 /api/index.php：multipart 字段 image + token
func (a *app) handleAPI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "3600")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		a.receive(w, r, true)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
