// PicNook 是一个不依赖 PHP 的自托管图床，兼容 EasyImage 的 /i/年/月/日/文件名 目录结构和上传 API，方便迁移。
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font/opentype"
)

//go:embed templates/*.html
var templateFS embed.FS

type config struct {
	listen    string
	imagesDir string // 对应网址中的 /i/
	trashDir  string
	configDir string // settings.json、上传日志和水印字体放在这里
	extraDirs []extraDir
	imageCORS bool // 图片响应是否带 Access-Control-Allow-Origin；反向代理已经加了时要关掉，否则会出现两个值
	baseURL   string
	password  string
	secret    []byte
	tokens    []string
	trusted   []*net.IPNet // 只信任这些地址发来的 X-Real-IP
}

// extraDir 是只读的旧图片目录，例如 EasyImage 早期版本用的 /images/
type extraDir struct {
	prefix string
	dir    string
}

// 这些网址前缀被程序自己占用，不能用作旧图片目录
var reservedPrefixes = map[string]bool{
	"i": true, "admin": true, "api": true, "app": true, "application": true, "login": true,
	"logout": true, "upload": true, "del": true, "healthz": true,
}

var extraPrefixRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type app struct {
	cfg     config
	tmpl    *template.Template
	limiter *loginLimiter
	secure  bool
	store   *settingsStore
	logMu   sync.Mutex
	procSem chan struct{} // 限制同时处理的图片数量，避免内存占用过高

	fontMu     sync.RWMutex
	font       *opentype.Font
	fontCustom bool
}

// envOr 让每个命令行参数都能用环境变量设置，方便 Docker 部署
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthcheck 供 Docker HEALTHCHECK 使用：镜像里没有 curl，用程序自己请求 /healthz
func healthcheck() {
	host, port, err := net.SplitHostPort(envOr("PICNOOK_LISTEN", "127.0.0.1:18321"))
	if err != nil {
		os.Exit(1)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

// resetPassword 清除后台改过的密码，重启后恢复使用 PICNOOK_PASSWORD 登录。
// 用法：picnook reset-password [配置目录]，Docker 里是 docker compose exec picnook /picnook reset-password
func resetPassword(args []string) {
	dir := envOr("PICNOOK_CONFIG_DIR", "")
	if len(args) > 0 {
		dir = args[0]
	}
	if dir == "" {
		log.Fatal("请指定配置目录：picnook reset-password <配置目录>（Docker 内为 /data/config）")
	}
	p := filepath.Join(dir, "settings.json")
	b, err := os.ReadFile(p)
	if err != nil {
		log.Fatalf("读取 %s 失败：%v", p, err)
	}
	// 用通用的 map 读写，只去掉密码字段，其他设置原样保留
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		log.Fatalf("解析 %s 失败：%v", p, err)
	}
	if _, ok := m["password_hash"]; !ok {
		fmt.Println("后台没有改过密码，当前使用的就是 PICNOOK_PASSWORD，无需重置")
		os.Exit(0)
	}
	delete(m, "password_hash")
	out, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = os.WriteFile(p, out, 0o600)
	}
	if err != nil {
		log.Fatalf("写入 %s 失败：%v", p, err)
	}
	fmt.Println("已清除后台设置的密码。重启 PicNook 后，用 PICNOOK_PASSWORD 登录")
	os.Exit(0)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck()
	}
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		resetPassword(os.Args[2:])
	}
	var cfg config
	var maxMB int64
	var trusted, extra, imageCORS string
	defaultMB, err := strconv.ParseInt(envOr("PICNOOK_MAX_MB", "10"), 10, 64)
	if err != nil {
		log.Fatal("PICNOOK_MAX_MB 必须是整数")
	}
	flag.StringVar(&cfg.listen, "listen", envOr("PICNOOK_LISTEN", "127.0.0.1:18321"), "监听地址（PICNOOK_LISTEN）")
	flag.StringVar(&cfg.imagesDir, "images", envOr("PICNOOK_IMAGES", ""), "图片目录，即原 EasyImage 站点下的 i 目录（PICNOOK_IMAGES）")
	flag.StringVar(&cfg.trashDir, "trash", envOr("PICNOOK_TRASH", ""), "删除的图片移到这里，默认是图片目录旁的 i-trash（PICNOOK_TRASH）")
	flag.StringVar(&cfg.configDir, "config-dir", envOr("PICNOOK_CONFIG_DIR", ""), "后台配置、上传日志和水印字体的目录，默认是图片目录旁的 picnook-config（PICNOOK_CONFIG_DIR）")
	flag.StringVar(&cfg.baseURL, "base", envOr("PICNOOK_BASE", ""), "图床网址，例如 https://img.example.com（PICNOOK_BASE）")
	flag.Int64Var(&maxMB, "max-mb", defaultMB, "单张图片大小上限的初始值，单位 MB，之后可在后台修改（PICNOOK_MAX_MB）")
	flag.StringVar(&imageCORS, "image-cors", envOr("PICNOOK_IMAGE_CORS", "on"), "图片响应是否带跨域头 on/off，反向代理已经加了时设为 off（PICNOOK_IMAGE_CORS）")
	flag.StringVar(&extra, "extra-dirs", envOr("PICNOOK_EXTRA_DIRS", ""), "只读的旧图片目录，格式 前缀=目录，逗号分隔，例如 images=/data/images（PICNOOK_EXTRA_DIRS）")
	flag.StringVar(&trusted, "trusted-proxies", envOr("PICNOOK_TRUSTED_PROXIES", "127.0.0.0/8,::1/128"), "反向代理地址段，逗号分隔（PICNOOK_TRUSTED_PROXIES）")
	flag.Parse()

	for _, s := range strings.Split(trusted, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			log.Fatalf("可信代理地址段无效：%s", s)
		}
		cfg.trusted = append(cfg.trusted, n)
	}

	switch strings.ToLower(strings.TrimSpace(imageCORS)) {
	case "on", "true", "1", "":
		cfg.imageCORS = true
	case "off", "false", "0":
	default:
		log.Fatalf("PICNOOK_IMAGE_CORS 只能是 on 或 off：%s", imageCORS)
	}

	for _, s := range strings.Split(extra, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		prefix, dir, ok := strings.Cut(s, "=")
		prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		if !ok || !extraPrefixRe.MatchString(prefix) || reservedPrefixes[prefix] || strings.TrimSpace(dir) == "" {
			log.Fatalf("旧图片目录配置无效：%s（格式 前缀=目录，前缀不能是 %s 等程序占用的名字）", s, "i、admin、api")
		}
		abs, err := filepath.Abs(strings.TrimSpace(dir))
		if err != nil {
			log.Fatal(err)
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			log.Fatalf("旧图片目录不存在：%s", abs)
		}
		cfg.extraDirs = append(cfg.extraDirs, extraDir{prefix: prefix, dir: abs})
	}

	cfg.password = os.Getenv("PICNOOK_PASSWORD")
	cfg.secret = []byte(os.Getenv("PICNOOK_SECRET"))
	for _, t := range strings.Split(os.Getenv("PICNOOK_TOKENS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.tokens = append(cfg.tokens, t)
		}
	}
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")

	if cfg.imagesDir == "" || cfg.baseURL == "" {
		log.Fatal("必须指定图片目录（-images / PICNOOK_IMAGES）和网址（-base / PICNOOK_BASE）")
	}
	if len(cfg.password) < 8 {
		log.Fatal("环境变量 PICNOOK_PASSWORD 至少 8 位")
	}
	if len(cfg.secret) < 32 {
		log.Fatal("环境变量 PICNOOK_SECRET 至少 32 个字符")
	}
	u, err := url.Parse(cfg.baseURL)
	if err != nil || u.Host == "" {
		log.Fatal("-base 不是有效网址")
	}
	if cfg.imagesDir, err = filepath.Abs(cfg.imagesDir); err != nil {
		log.Fatal(err)
	}
	if st, err := os.Stat(cfg.imagesDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.imagesDir, 0o755); err != nil {
			log.Fatalf("创建图片目录失败：%v", err)
		}
		log.Printf("图片目录不存在，已新建：%s", cfg.imagesDir)
	} else if err != nil || !st.IsDir() {
		log.Fatalf("图片目录无效：%s", cfg.imagesDir)
	}
	if cfg.trashDir == "" {
		cfg.trashDir = filepath.Join(filepath.Dir(cfg.imagesDir), "i-trash")
	}
	if cfg.configDir == "" {
		cfg.configDir = filepath.Join(filepath.Dir(cfg.imagesDir), "picnook-config")
	}
	if err := os.MkdirAll(cfg.configDir, 0o700); err != nil {
		log.Fatalf("创建配置目录失败：%v", err)
	}

	defaults := Settings{
		MaxMB: maxMB, Quality: 85, WaterText: u.Host, WaterPos: 9, WaterSize: 4,
		WaterColor: "#ffffff", WaterOpacity: 60, AllowEmptyRef: true,
	}
	// 首次启动时把环境变量里的 token 导入后台，之后统一在后台管理
	for i, t := range cfg.tokens {
		defaults.Tokens = append(defaults.Tokens, Token{ID: i + 1, Value: t, Created: time.Now().Unix()})
	}
	store, first, err := loadSettings(filepath.Join(cfg.configDir, "settings.json"), defaults)
	if err != nil {
		log.Fatalf("读取配置失败：%v", err)
	}
	if !first && len(cfg.tokens) > 0 {
		log.Printf("提示：配置已存在，PICNOOK_TOKENS 只在首次启动时导入，请在后台管理 token")
	}

	a := &app{
		cfg:     cfg,
		tmpl:    template.Must(template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")),
		limiter: newLoginLimiter(),
		secure:  u.Scheme == "https",
		store:   store,
		procSem: make(chan struct{}, 2),
	}
	a.loadFont()
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("picnook 监听 %s，图片目录 %s，配置目录 %s", cfg.listen, cfg.imagesDir, cfg.configDir)
	for _, e := range cfg.extraDirs {
		log.Printf("旧图片目录（只读）：/%s/ -> %s", e.prefix, e.dir)
	}
	log.Fatal(srv.ListenAndServe())
}

// 兼容 EasyImage 开启 hide_path 后生成的 /年/月/日/文件名 链接
var hiddenPathRe = regexp.MustCompile(`^/(\d{4}/\d{2}/\d{2}/[^/]+)$`)

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			a.handleIndex(w, r)
			return
		}
		if m := hiddenPathRe.FindStringSubmatch(r.URL.Path); m != nil {
			a.serveImage(w, r, a.cfg.imagesDir, m[1])
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/i/", func(w http.ResponseWriter, r *http.Request) {
		a.serveImage(w, r, a.cfg.imagesDir, strings.TrimPrefix(r.URL.Path, "/i/"))
	})
	for _, e := range a.cfg.extraDirs {
		e := e
		mux.HandleFunc("/"+e.prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			a.serveImage(w, r, e.dir, strings.TrimPrefix(r.URL.Path, "/"+e.prefix+"/"))
		})
	}
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/upload", a.handleUpload)
	mux.HandleFunc("/api/index.php", a.handleAPI)
	mux.HandleFunc("/api/upload", a.handleAPI)
	mux.HandleFunc("/admin", a.handleAdmin)
	mux.HandleFunc("/admin/delete", a.handleAdminDelete)
	mux.HandleFunc("/admin/index.php", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/admin/settings", a.handleSettings)
	mux.HandleFunc("/admin/settings/preview", a.handleWatermarkPreview)
	mux.HandleFunc("/admin/font", a.handleFont)
	mux.HandleFunc("/admin/password", a.handlePassword)
	mux.HandleFunc("/admin/tokens", a.handleTokens)
	mux.HandleFunc("/admin/tokens/import", a.handleTokenImport)
	mux.HandleFunc("/admin/tokens/files", a.handleTokenFiles)
	mux.HandleFunc("/del", a.handleSignedDelete)
	mux.HandleFunc("/app/thumb.php", a.handleThumbRedirect)
	mux.HandleFunc("/application/thumb.php", a.handleThumbRedirect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func (a *app) render(w http.ResponseWriter, status int, name string, data interface{}) {
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("渲染 %s 失败：%v", name, err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !a.loggedIn(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s := a.store.get()
	a.render(w, http.StatusOK, "index.html", map[string]interface{}{
		"MaxMB":     s.MaxMB,
		"Compress":  s.Compress,
		"WebP":      s.Convert == "webp",
		"Watermark": s.Watermark && s.WaterText != "",
		"Recent":    a.recentImages(12),
	})
}
