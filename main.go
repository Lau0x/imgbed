// imgbed 是 EasyImage 的替代品：不依赖 PHP，沿用原有的 /i/年/月/日/文件名 目录结构和上传 API。
package main

import (
	"bytes"
	"embed"
	"flag"
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
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

type config struct {
	listen    string
	imagesDir string // 对应网址中的 /i/
	trashDir  string
	baseURL   string
	maxBytes  int64
	password  string
	secret    []byte
	tokens    []string
	trusted   []*net.IPNet // 只信任这些地址发来的 X-Real-IP
}

type app struct {
	cfg     config
	tmpl    *template.Template
	limiter *loginLimiter
	secure  bool
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
	host, port, err := net.SplitHostPort(envOr("IMGBED_LISTEN", "127.0.0.1:18321"))
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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck()
	}
	var cfg config
	var maxMB int64
	var trusted string
	defaultMB, err := strconv.ParseInt(envOr("IMGBED_MAX_MB", "10"), 10, 64)
	if err != nil {
		log.Fatal("IMGBED_MAX_MB 必须是整数")
	}
	flag.StringVar(&cfg.listen, "listen", envOr("IMGBED_LISTEN", "127.0.0.1:18321"), "监听地址（IMGBED_LISTEN）")
	flag.StringVar(&cfg.imagesDir, "images", envOr("IMGBED_IMAGES", ""), "图片目录，即原 EasyImage 站点下的 i 目录（IMGBED_IMAGES）")
	flag.StringVar(&cfg.trashDir, "trash", envOr("IMGBED_TRASH", ""), "删除的图片移到这里，默认是图片目录旁的 i-trash（IMGBED_TRASH）")
	flag.StringVar(&cfg.baseURL, "base", envOr("IMGBED_BASE", ""), "图床网址，例如 https://img.example.com（IMGBED_BASE）")
	flag.Int64Var(&maxMB, "max-mb", defaultMB, "单张图片大小上限，单位 MB（IMGBED_MAX_MB）")
	flag.StringVar(&trusted, "trusted-proxies", envOr("IMGBED_TRUSTED_PROXIES", "127.0.0.0/8,::1/128"), "反向代理地址段，逗号分隔（IMGBED_TRUSTED_PROXIES）")
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

	cfg.password = os.Getenv("IMGBED_PASSWORD")
	cfg.secret = []byte(os.Getenv("IMGBED_SECRET"))
	for _, t := range strings.Split(os.Getenv("IMGBED_TOKENS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.tokens = append(cfg.tokens, t)
		}
	}
	cfg.maxBytes = maxMB << 20
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")

	if cfg.imagesDir == "" || cfg.baseURL == "" {
		log.Fatal("必须指定图片目录（-images / IMGBED_IMAGES）和网址（-base / IMGBED_BASE）")
	}
	if len(cfg.password) < 8 {
		log.Fatal("环境变量 IMGBED_PASSWORD 至少 8 位")
	}
	if len(cfg.secret) < 32 {
		log.Fatal("环境变量 IMGBED_SECRET 至少 32 个字符")
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

	a := &app{
		cfg:     cfg,
		tmpl:    template.Must(template.ParseFS(templateFS, "templates/*.html")),
		limiter: newLoginLimiter(),
		secure:  u.Scheme == "https",
	}
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("imgbed 监听 %s，图片目录 %s，API Token %d 个", cfg.listen, cfg.imagesDir, len(cfg.tokens))
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
			a.serveImage(w, r, m[1])
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/i/", func(w http.ResponseWriter, r *http.Request) {
		a.serveImage(w, r, strings.TrimPrefix(r.URL.Path, "/i/"))
	})
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
	a.render(w, http.StatusOK, "index.html", map[string]interface{}{
		"MaxMB": a.cfg.maxBytes >> 20,
	})
}
