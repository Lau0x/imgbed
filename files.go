package main

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gen2brain/webp"
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
	if a.cfg.imageCORS {
		h.Set("Access-Control-Allow-Origin", "*")
	}
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

// monthGroup 把日期列表按月分组，供管理页的日期条使用
type monthGroup struct {
	Key   string // 2026/10
	Label string // 2026 年 10 月
	Days  []dayLink
}

type dayLink struct {
	Value string // 2026/10/23
	Label string // 23 日
	Num   string // 23
	Count int    // 当天的图片数量
}

// dayGroups 按月分组并统计每天的图片数量
func (a *app) dayGroups(days []string) []monthGroup {
	groups := dayGroups(days)
	for gi := range groups {
		for di := range groups[gi].Days {
			groups[gi].Days[di].Count = a.countDay(groups[gi].Days[di].Value)
		}
	}
	return groups
}

func (a *app) countDay(day string) int {
	entries, err := os.ReadDir(filepath.Join(a.cfg.imagesDir, filepath.FromSlash(day)))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") && imageType(e.Name()) != "" {
			n++
		}
	}
	return n
}

func dayGroups(days []string) []monthGroup {
	var out []monthGroup
	for _, d := range days {
		if len(d) != 10 {
			continue
		}
		key := d[:7]
		if len(out) == 0 || out[len(out)-1].Key != key {
			out = append(out, monthGroup{Key: key, Label: d[:4] + " 年 " + d[5:7] + " 月"})
		}
		g := &out[len(out)-1]
		g.Days = append(g.Days, dayLink{Value: d, Label: d[8:] + " 日", Num: strings.TrimPrefix(d[8:], "0")})
	}
	return out
}

type imageItem struct {
	Rel   string
	URL   string
	Name  string
	Size  string
	Time  string
	W, H  int // 图片尺寸，取不到时为 0
	bytes int64
	mod   time.Time
}

// imageDims 只读取文件头获取尺寸，用于瀑布流提前占位
func imageDims(p string) (int, int) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var cfg image.Config
	if strings.EqualFold(path.Ext(p), ".webp") {
		cfg, err = webp.DecodeConfig(f)
	} else {
		cfg, _, err = image.DecodeConfig(f)
	}
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
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
		w, h := imageDims(filepath.Join(a.cfg.imagesDir, filepath.FromSlash(rel)))
		items = append(items, imageItem{
			Rel:   rel,
			URL:   a.cfg.baseURL + "/i/" + rel,
			Name:  name,
			Size:  humanSize(info.Size()),
			Time:  info.ModTime().Format("15:04:05"),
			W:     w,
			H:     h,
			bytes: info.Size(),
			mod:   info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	return items
}

// recentImages 返回最近上传的 n 张图片，从最新的日期往前找
func (a *app) recentImages(n int) []imageItem {
	var out []imageItem
	for _, d := range a.recentDays(60) {
		for _, it := range a.listDay(d) {
			out = append(out, it)
			if len(out) >= n {
				return out
			}
		}
	}
	return out
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
	var total int64
	for _, it := range images {
		total += it.bytes
	}
	// days 按时间倒序：前一天是更早的日期，后一天是更新的日期
	var prev, next string
	for i, d := range days {
		if d == day {
			if i+1 < len(days) {
				prev = days[i+1]
			}
			if i > 0 {
				next = days[i-1]
			}
		}
	}
	a.render(w, r, http.StatusOK, "admin.html", map[string]interface{}{
		"Groups": a.dayGroups(days), "Day": day, "Images": images, "Msg": r.URL.Query().Get("msg"),
		"Total": humanSize(total), "Prev": prev, "Next": next,
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
	if err := moveFile(src, dst); err != nil {
		return err
	}
	log.Printf("已删除 %s（移至 %s）", rel, dst)
	return nil
}

// moveFile 先尝试直接改名；图片目录和回收目录是两个挂载点（Docker 里常见）时改名会失败，改为复制后再删除原文件
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".trash-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), st.Mode().Perm())
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return err
	}
	os.Chtimes(dst, st.ModTime(), st.ModTime())
	return os.Remove(src)
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
	r.ParseForm()
	rels := r.PostForm["f"]
	if len(rels) == 0 {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	var msg string
	if len(rels) == 1 {
		msg = a.T(r, deleteMessage(a.trash(rels[0])))
	} else {
		ok := 0
		for _, rel := range rels {
			if a.trash(rel) == nil {
				ok++
			}
		}
		msg = a.T(r, "已删除 %d 张图片（已移到回收目录）", ok)
		if ok < len(rels) {
			msg += a.T(r, "，%d 张删除失败", len(rels)-ok)
		}
	}
	back := r.PostFormValue("back")
	if !strings.HasPrefix(back, "/admin") || strings.HasPrefix(back, "//") {
		back = "/admin?d=" + url.QueryEscape(path.Dir(rels[0]))
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+"msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleSignedDelete 处理 API 返回的 del 链接：GET 只显示确认页，POST 才真正删除
func (a *app) handleSignedDelete(w http.ResponseWriter, r *http.Request) {
	rel, sig := r.FormValue("f"), r.FormValue("s")
	if !hmac.Equal([]byte(sig), []byte(a.sign("del|"+rel))) {
		a.render(w, r, http.StatusForbidden, "del.html", map[string]string{"Msg": a.T(r, "删除链接无效")})
		return
	}
	data := map[string]string{"F": rel, "S": sig, "URL": a.cfg.baseURL + "/i/" + rel}
	switch r.Method {
	case http.MethodGet:
		a.render(w, r, http.StatusOK, "del.html", data)
	case http.MethodPost:
		if !sameOrigin(r) {
			http.Error(w, "禁止访问", http.StatusForbidden)
			return
		}
		a.render(w, r, http.StatusOK, "del.html", map[string]string{"Msg": a.T(r, deleteMessage(a.trash(rel)))})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
