package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// 界面支持中文和英文。模板和提示信息里直接写中文原文，英文版通过 enDict 查表替换，查不到时回退到中文。
var langs = []string{"zh", "en"}

// langOf 决定当前请求使用的语言：先看手动切换后写入的 lang cookie，再看浏览器语言
func langOf(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil && (c.Value == "zh" || c.Value == "en") {
		return c.Value
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case tag == "" || tag == "*":
			continue
		case strings.HasPrefix(tag, "zh"):
			return "zh"
		default:
			return "en"
		}
	}
	return "zh"
}

// tr 翻译一条文字；带参数时把译文当作 fmt 格式串
func tr(lang, key string, args ...interface{}) string {
	s := key
	if lang == "en" {
		if v, ok := enDict[key]; ok {
			s = v
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// T 按请求的语言翻译，用于服务端生成的提示信息
func (a *app) T(r *http.Request, key string, args ...interface{}) string {
	return tr(langOf(r), key, args...)
}

// buildTemplates 为每种语言各解析一套模板，模板函数 t 绑定对应语言
func buildTemplates() map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, l := range langs {
		l := l
		funcs := template.FuncMap{}
		for k, v := range templateFuncs {
			funcs[k] = v
		}
		funcs["t"] = func(key string, args ...interface{}) string { return tr(l, key, args...) }
		funcs["lang"] = func() string { return l }
		funcs["htmlLang"] = func() string {
			if l == "en" {
				return "en"
			}
			return "zh-CN"
		}
		// dayLabel 把 2026/10/23 显示成 2026 年 10 月 23 日 / Oct 23, 2026
		funcs["dayLabel"] = func(d string) string {
			t, err := time.Parse("2006/01/02", d)
			if err != nil {
				return d
			}
			if l == "en" {
				return t.Format("Jan 2, 2006")
			}
			return fmt.Sprintf("%d 年 %d 月 %d 日", t.Year(), int(t.Month()), t.Day())
		}
		// monthLabel 把 2026/10 显示成 2026 年 10 月 / October 2026
		funcs["monthLabel"] = func(m string) string {
			t, err := time.Parse("2006/01", m)
			if err != nil {
				return m
			}
			if l == "en" {
				return t.Format("January 2006")
			}
			return fmt.Sprintf("%d 年 %d 月", t.Year(), int(t.Month()))
		}
		out[l] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	}
	return out
}
