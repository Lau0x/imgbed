package main

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var contentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".avif": "image/avif",
	".svg":  "image/svg+xml",
}

var dayRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}$`)

// cleanRel 校验相对路径：不允许 ..、绝对路径、反斜杠和以点开头的段
func cleanRel(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	if path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg[0] == '.' {
			return "", false
		}
	}
	return p, true
}

func imageType(name string) string {
	return contentTypes[strings.ToLower(path.Ext(name))]
}

// serveImage 输出 root 目录下的图片；按推荐配置部署时 /i/ 由 Nginx 直接返回静态文件，不经过这里
func (a *app) serveImage(w http.ResponseWriter, r *http.Request, root, rel string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rel, ok := cleanRel(rel)
	ctype := imageType(rel)
	if !ok || ctype == "" {
		http.NotFound(w, r)
		return
	}
	if s := a.store.get(); s.Hotlink {
		w.Header().Set("Vary", "Referer")
		if !a.refererAllowed(r.Header.Get("Referer"), s) {
			http.Error(w, "禁止盗链", http.StatusForbidden)
			return
		}
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=2592000")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Del("X-Frame-Options")
	if ctype == "image/svg+xml" {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// normalizeReferer 把后台填的 https://a.com/path、A.com 等写法统一成小写域名
func normalizeReferer(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if i := strings.Index(p, "://"); i >= 0 {
		p = p[i+3:]
	}
	if i := strings.IndexAny(p, "/:"); i >= 0 {
		p = p[:i]
	}
	return p
}

// refererAllowed 防盗链判断：本站、白名单域名（支持 *.example.com）放行，空 Referer 按设置决定
func (a *app) refererAllowed(ref string, s Settings) bool {
	if ref == "" {
		return s.AllowEmptyRef
	}
	u, err := url.Parse(ref)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if base, err := url.Parse(a.cfg.baseURL); err == nil && strings.EqualFold(base.Hostname(), host) {
		return true
	}
	for _, p := range s.AllowedRefs {
		if strings.HasPrefix(p, "*.") {
			if host == p[2:] || strings.HasSuffix(host, p[1:]) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// handleThumbRedirect 把旧的 thumb.php?img=/i/... 缩略图链接跳到原图
func (a *app) handleThumbRedirect(w http.ResponseWriter, r *http.Request) {
	img := r.URL.Query().Get("img")
	if rel, ok := cleanRel(strings.TrimPrefix(img, "/i/")); ok && strings.HasPrefix(img, "/i/") && imageType(rel) != "" {
		http.Redirect(w, r, "/i/"+rel, http.StatusMovedPermanently)
		return
	}
	http.NotFound(w, r)
}

func digitDirs(dir string, width int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || len(name) != width || strings.Trim(name, "0123456789") != "" {
			continue
		}
		out = append(out, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

func (a *app) recentDays(limit int) []string {
	var days []string
	for _, y := range digitDirs(a.cfg.imagesDir, 4) {
		for _, m := range digitDirs(filepath.Join(a.cfg.imagesDir, y), 2) {
			for _, d := range digitDirs(filepath.Join(a.cfg.imagesDir, y, m), 2) {
				days = append(days, y+"/"+m+"/"+d)
				if len(days) >= limit {
					return days
				}
			}
		}
	}
	return days
}

type imageItem struct {
	Rel  string
	URL  string
	Name string
	Size string
	Time string
	mod  time.Time
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (a *app) listDay(day string) []imageItem {
	entries, err := os.ReadDir(filepath.Join(a.cfg.imagesDir, filepath.FromSlash(day)))
	if err != nil {
		return nil
	}
	var items []imageItem
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || imageType(name) == "" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel := day + "/" + name
		items = append(items, imageItem{
			Rel:  rel,
			URL:  a.cfg.baseURL + "/i/" + rel,
			Name: name,
			Size: humanSize(info.Size()),
			Time: info.ModTime().Format("15:04:05"),
			mod:  info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	return items
}

func (a *app) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !a.loggedIn(r) {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/admin"), http.StatusSeeOther)
		return
	}
	days := a.recentDays(180)
	day := r.URL.Query().Get("d")
	if day == "" && len(days) > 0 {
		day = days[0]
	}
	var images []imageItem
	if dayRe.MatchString(day) {
		images = a.listDay(day)
	}
	a.render(w, http.StatusOK, "admin.html", map[string]interface{}{
		"Days": days, "Day": day, "Images": images, "Msg": r.URL.Query().Get("msg"),
	})
}

var errBadPath = errors.New("路径无效")

// trash 把图片移到回收目录而不是直接删除，误删可以手动恢复
func (a *app) trash(rel string) error {
	rel, ok := cleanRel(rel)
	if !ok || imageType(rel) == "" {
		return errBadPath
	}
	src := filepath.Join(a.cfg.imagesDir, filepath.FromSlash(rel))
	st, err := os.Lstat(src)
	if err != nil || !st.Mode().IsRegular() {
		return os.ErrNotExist
	}
	dst := filepath.Join(a.cfg.trashDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		dst += "." + time.Now().Format("20060102150405")
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	log.Printf("已删除 %s（移至 %s）", rel, dst)
	return nil
}

func deleteMessage(err error) string {
	switch {
	case err == nil:
		return "已删除"
	case errors.Is(err, os.ErrNotExist):
		return "图片不存在或已删除"
	case errors.Is(err, errBadPath):
		return "路径无效"
	}
	log.Printf("删除失败：%v", err)
	return "删除失败"
}

func (a *app) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.loggedIn(r) || !sameOrigin(r) {
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	rel := r.PostFormValue("f")
	msg := deleteMessage(a.trash(rel))
	http.Redirect(w, r, "/admin?d="+url.QueryEscape(path.Dir(rel))+"&msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleSignedDelete 处理 API 返回的 del 链接：GET 只显示确认页，POST 才真正删除
func (a *app) handleSignedDelete(w http.ResponseWriter, r *http.Request) {
	rel, sig := r.FormValue("f"), r.FormValue("s")
	if !hmac.Equal([]byte(sig), []byte(a.sign("del|"+rel))) {
		a.render(w, http.StatusForbidden, "del.html", map[string]string{"Msg": "删除链接无效"})
		return
	}
	data := map[string]string{"F": rel, "S": sig, "URL": a.cfg.baseURL + "/i/" + rel}
	switch r.Method {
	case http.MethodGet:
		a.render(w, http.StatusOK, "del.html", data)
	case http.MethodPost:
		if !sameOrigin(r) {
			http.Error(w, "禁止访问", http.StatusForbidden)
			return
		}
		a.render(w, http.StatusOK, "del.html", map[string]string{"Msg": deleteMessage(a.trash(rel))})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
