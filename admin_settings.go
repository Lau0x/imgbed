package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var templateFuncs = template.FuncMap{
	"date": func(ts int64) string {
		if ts == 0 {
			return ""
		}
		return time.Unix(ts, 0).Format("2006-01-02 15:04")
	},
	"day": func(ts int64) string {
		if ts == 0 {
			return ""
		}
		return time.Unix(ts, 0).Format("2006-01-02")
	},
	// mask 只显示 token 的首尾几位，完整值通过复制按钮获取
	"mask": func(s string) string {
		if len(s) <= 12 {
			return s
		}
		return s[:6] + "••••••" + s[len(s)-4:]
	},
}

var waterPositions = []struct {
	V int
	L string
}{{1, "左上"}, {2, "上中"}, {3, "右上"}, {4, "左中"}, {5, "正中"}, {6, "右中"}, {7, "左下"}, {8, "下中"}, {9, "右下"}}

var (
	tokenValueRe = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
	// 匹配 EasyImage config/api_key.php 里的一条 token
	easyImageTokenRe = regexp.MustCompile(`(?i)'([a-z0-9]{16,64})'\s*=>\s*array\s*\(\s*'id'\s*=>\s*(\d+)\s*,\s*'expired'\s*=>\s*(\d+)(?:\s*,\s*'add_time'\s*=>\s*(\d+))?`)
)

type tokenRow struct {
	Token
	Uploads int
	Status  string
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// requireAdmin 校验登录；写操作还要求 POST 且来自本站页面
func (a *app) requireAdmin(w http.ResponseWriter, r *http.Request, write bool) bool {
	if !a.loggedIn(r) {
		if write {
			http.Error(w, "禁止访问", http.StatusForbidden)
		} else {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		}
		return false
	}
	if write && (r.Method != http.MethodPost || !sameOrigin(r)) {
		http.Error(w, "禁止访问", http.StatusForbidden)
		return false
	}
	return true
}

// backTo 带着提示回到设置页；msg 按当前语言翻译（已经翻译过的格式化文字原样保留）
func backTo(w http.ResponseWriter, r *http.Request, msg, anchor string) {
	msg = tr(langOf(r), msg)
	http.Redirect(w, r, "/admin/settings?msg="+url.QueryEscape(msg)+"#"+anchor, http.StatusSeeOther)
}

func formInt(r *http.Request, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(key)))
	if err != nil {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (a *app) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.saveSettings(w, r)
		return
	}
	if !a.requireAdmin(w, r, false) {
		return
	}
	s := a.store.get()
	counts := map[string]int{}
	for _, e := range a.readUploadLog() {
		if e.Token != "" {
			counts[e.Token]++
		}
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	now := time.Now()
	var rows []tokenRow
	for _, t := range s.Tokens {
		if q != "" && !strings.Contains(t.Value, q) && !strings.Contains(t.Note, q) && strconv.Itoa(t.ID) != q {
			continue
		}
		row := tokenRow{Token: t, Uploads: counts[tokenHash(t.Value)], Status: "正常"}
		if t.Disabled {
			row.Status = "已禁用"
		} else if t.expired(now) {
			row.Status = "已过期"
		}
		rows = append(rows, row)
	}
	_, custom := a.currentFont()
	a.render(w, r, http.StatusOK, "settings.html", map[string]interface{}{
		"S":           s,
		"Refs":        strings.Join(s.AllowedRefs, "\n"),
		"Tokens":      rows,
		"TokenTotal":  len(s.Tokens),
		"Q":           q,
		"NewToken":    randomToken(),
		"APIURL":      a.cfg.baseURL + "/api/index.php",
		"CustomFont":  custom,
		"Missing":     a.missingGlyphs(s.WaterText),
		"Positions":   waterPositions,
		"Msg":         r.URL.Query().Get("msg"),
		"Now":         now.Unix(),
		"EnvPassword": s.PasswordHash == "",
	})
}

func (a *app) saveSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, true) {
		return
	}
	anchor := r.PostFormValue("anchor")
	err := a.store.update(func(s *Settings) error {
		text := strings.TrimSpace(r.PostFormValue("water_text"))
		if utf8.RuneCountInString(text) > 60 {
			return errors.New("水印文字不能超过 60 个字")
		}
		s.MaxMB = int64(formInt(r, "max_mb", int(s.MaxMB), 1, 500))
		s.Compress = r.PostFormValue("compress") == "on"
		s.Quality = formInt(r, "quality", s.Quality, 40, 100)
		s.Convert = ""
		if r.PostFormValue("convert") == "webp" {
			s.Convert = "webp"
		}
		s.Watermark = r.PostFormValue("watermark") == "on"
		s.WaterText = text
		s.WaterPos = formInt(r, "water_pos", 9, 1, 9)
		s.WaterSize = formInt(r, "water_size", 4, 1, 20)
		if c := r.PostFormValue("water_color"); c != "" {
			if _, ok := parseHexColor(c); ok {
				s.WaterColor = strings.ToLower(c)
			}
		}
		s.WaterOpacity = formInt(r, "water_opacity", 60, 0, 100)
		s.Hotlink = r.PostFormValue("hotlink") == "on"
		s.AllowEmptyRef = r.PostFormValue("allow_empty_referer") == "on"
		s.AllowedRefs = nil
		seen := map[string]bool{}
		for _, p := range strings.FieldsFunc(r.PostFormValue("allowed_referers"), func(c rune) bool {
			return c == '\n' || c == '\r' || c == ',' || c == ' '
		}) {
			if n := normalizeReferer(p); n != "" && !seen[n] {
				seen[n] = true
				s.AllowedRefs = append(s.AllowedRefs, n)
			}
		}
		return nil
	})
	if err != nil {
		backTo(w, r, a.T(r, "保存失败：%s", a.T(r, err.Error())), anchor)
		return
	}
	backTo(w, r, "设置已保存", anchor)
}

func (a *app) handleTokens(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, true) {
		return
	}
	value := strings.TrimSpace(r.PostFormValue("value"))
	var msg string
	var err error
	switch r.PostFormValue("action") {
	case "add":
		if !tokenValueRe.MatchString(value) {
			backTo(w, r, "Token 只能由字母和数字组成，长度 16–64 位", "tokens")
			return
		}
		days := formInt(r, "days", 0, 0, 36500)
		note := strings.TrimSpace(r.PostFormValue("note"))
		if utf8.RuneCountInString(note) > 50 {
			note = string([]rune(note)[:50])
		}
		var expires int64
		if days > 0 {
			expires = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
		}
		err = a.store.update(func(s *Settings) error {
			maxID := 0
			for i := range s.Tokens {
				if s.Tokens[i].ID > maxID {
					maxID = s.Tokens[i].ID
				}
				// 和 EasyImage 一样：已存在的 token 视为修改有效期
				if s.Tokens[i].Value == value {
					s.Tokens[i].Expires, s.Tokens[i].Disabled = expires, false
					if note != "" {
						s.Tokens[i].Note = note
					}
					msg = a.T(r, "已修改 Token #%d 的有效期", s.Tokens[i].ID)
					return nil
				}
			}
			s.Tokens = append(s.Tokens, Token{ID: maxID + 1, Value: value, Note: note, Created: time.Now().Unix(), Expires: expires})
			msg = a.T(r, "已添加 Token #%d", maxID+1)
			return nil
		})
	case "disable", "enable":
		disable := r.PostFormValue("action") == "disable"
		err = a.store.update(func(s *Settings) error {
			for i := range s.Tokens {
				if s.Tokens[i].Value == value {
					s.Tokens[i].Disabled = disable
					return nil
				}
			}
			return errors.New("Token 不存在")
		})
		msg = map[bool]string{true: "已禁用", false: "已启用"}[disable]
	case "delete":
		err = a.store.update(func(s *Settings) error {
			for i := range s.Tokens {
				if s.Tokens[i].Value == value {
					s.Tokens = append(s.Tokens[:i], s.Tokens[i+1:]...)
					return nil
				}
			}
			return errors.New("Token 不存在")
		})
		msg = "已删除 Token，用它上传的图片仍然保留"
	case "purge":
		n := 0
		for _, rel := range a.tokenFiles(tokenHash(value)) {
			if a.trash(rel) == nil {
				n++
			}
		}
		msg = a.T(r, "已删除该 Token 上传的 %d 张图片（已移到回收目录）", n)
	default:
		err = errors.New("未知操作")
	}
	if err != nil {
		backTo(w, r, a.T(r, "操作失败：%s", a.T(r, err.Error())), "tokens")
		return
	}
	backTo(w, r, msg, "tokens")
}

// handleTokenImport 导入 EasyImage 的 config/api_key.php，保留原来的 ID、添加时间和有效期
func (a *app) handleTokenImport(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, true) {
		return
	}
	matches := easyImageTokenRe.FindAllStringSubmatch(r.PostFormValue("php"), -1)
	if len(matches) == 0 {
		backTo(w, r, "没有识别到 Token，请粘贴 config/api_key.php 的完整内容", "tokens")
		return
	}
	added, skipped := 0, 0
	err := a.store.update(func(s *Settings) error {
		exists := map[string]bool{}
		for _, t := range s.Tokens {
			exists[t.Value] = true
		}
		for _, m := range matches {
			if exists[m[1]] {
				skipped++
				continue
			}
			id, _ := strconv.Atoi(m[2])
			expires, _ := strconv.ParseInt(m[3], 10, 64)
			created, _ := strconv.ParseInt(m[4], 10, 64)
			if created == 0 {
				created = time.Now().Unix()
			}
			s.Tokens = append(s.Tokens, Token{ID: id, Value: m[1], Created: created, Expires: expires})
			exists[m[1]] = true
			added++
		}
		return nil
	})
	if err != nil {
		backTo(w, r, a.T(r, "导入失败：%s", a.T(r, err.Error())), "tokens")
		return
	}
	backTo(w, r, a.T(r, "导入 %d 个 Token，跳过已存在的 %d 个", added, skipped), "tokens")
}

// tokenFiles 返回某个 token 上传、且现在仍然存在的图片
func (a *app) tokenFiles(hash string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range a.readUploadLog() {
		if e.Token != hash || seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		if rel, ok := cleanRel(e.Path); ok {
			if st, err := os.Lstat(filepath.Join(a.cfg.imagesDir, filepath.FromSlash(rel))); err == nil && st.Mode().IsRegular() {
				out = append(out, rel)
			}
		}
	}
	return out
}

func (a *app) handleTokenFiles(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, false) {
		return
	}
	value := r.URL.Query().Get("t")
	var tok *Token
	for _, t := range a.store.get().Tokens {
		if t.Value == value {
			t := t
			tok = &t
		}
	}
	if tok == nil {
		http.NotFound(w, r)
		return
	}
	var items []imageItem
	for _, rel := range a.tokenFiles(tokenHash(value)) {
		info, err := os.Stat(filepath.Join(a.cfg.imagesDir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		items = append(items, imageItem{
			Rel: rel, URL: a.cfg.baseURL + "/i/" + rel, Name: rel, Size: humanSize(info.Size()),
			Time: info.ModTime().Format("2006-01-02 15:04"), mod: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	a.render(w, r, http.StatusOK, "admin.html", map[string]interface{}{
		"Groups": a.dayGroups(a.recentDays(180)),
		"Day":    "",
		"Title":  a.T(r, "Token #%d 上传的图片", tok.ID),
		"Extra":  template.HTML(`<a href="/admin/settings#tokens">` + template.HTMLEscapeString(a.T(r, "返回 Token 管理")) + `</a>`),
		"Images": items,
	})
}

func (a *app) handleFont(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 40<<20)
	if !a.requireAdmin(w, r, true) {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		backTo(w, r, "字体文件过大（上限 40MB）", "watermark")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if r.PostFormValue("action") == "delete" {
		os.Remove(a.fontPath())
		a.loadFont()
		backTo(w, r, "已恢复内置字体", "watermark")
		return
	}
	file, _, err := r.FormFile("font")
	if err != nil {
		backTo(w, r, "请选择字体文件", "watermark")
		return
	}
	defer file.Close()
	b, err := io.ReadAll(file)
	if err == nil {
		_, err = parseFont(b)
	}
	if err != nil {
		backTo(w, r, "字体无法识别，请上传 ttf / otf / ttc 文件", "watermark")
		return
	}
	tmp, err := os.CreateTemp(a.cfg.configDir, ".font-*")
	if err == nil {
		_, err = tmp.Write(b)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), a.fontPath())
		}
		os.Remove(tmp.Name())
	}
	if err != nil {
		log.Printf("保存字体失败：%v", err)
		backTo(w, r, "保存字体失败", "watermark")
		return
	}
	a.loadFont()
	backTo(w, r, "字体已更新", "watermark")
}

// handleWatermarkPreview 用当前设置在一张示例图上画水印，方便调整位置、大小和字体
func (a *app) handleWatermarkPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, false) {
		return
	}
	s := a.store.get()
	if s.WaterText == "" {
		s.WaterText = "示例水印 Sample"
	}
	const pw, ph = 800, 450
	img := image.NewNRGBA(image.Rect(0, 0, pw, ph))
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			v := uint8(215 - 150*x/pw) // 左浅右深，默认右下角的白色水印更容易看清
			img.SetNRGBA(x, y, color.NRGBA{v, v + 10, v + 25, 255})
		}
	}
	a.drawWatermark(img, s)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	png.Encode(w, img)
}

// handlePassword 修改后台登录密码：校验当前密码后保存新密码的加盐哈希，其他设备上的登录随之失效
func (a *app) handlePassword(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r, true) {
		return
	}
	ip := a.clientIP(r)
	if a.limiter.blocked(ip) {
		backTo(w, r, "失败次数过多，请 15 分钟后再试", "account")
		return
	}
	if !a.checkPassword(r.PostFormValue("current")) {
		a.limiter.fail(ip)
		backTo(w, r, "当前密码不正确", "account")
		return
	}
	next := r.PostFormValue("new")
	if err := checkPasswordPolicy(next); err != nil {
		backTo(w, r, err.Error(), "account")
		return
	}
	if next != r.PostFormValue("confirm") {
		backTo(w, r, "两次输入的新密码不一致", "account")
		return
	}
	hash, err := hashPassword(next)
	if err == nil {
		err = a.store.update(func(s *Settings) error {
			s.PasswordHash = hash
			return nil
		})
	}
	if err != nil {
		log.Printf("保存新密码失败：%v", err)
		backTo(w, r, "保存新密码失败", "account")
		return
	}
	a.limiter.reset(ip)
	log.Printf("后台密码已修改（来源 %s）", ip)
	a.setSession(w) // 当前设备换发新的登录状态，其他设备需要重新登录
	backTo(w, r, "密码已修改，其他设备上的登录已失效", "account")
}
