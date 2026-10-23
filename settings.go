package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token 对应 EasyImage 的 API Token，字段含义与 config/api_key.php 一致
type Token struct {
	ID       int    `json:"id"`
	Value    string `json:"value"`
	Note     string `json:"note,omitempty"`
	Created  int64  `json:"created"`
	Expires  int64  `json:"expires"` // Unix 秒，0 表示永不过期
	Disabled bool   `json:"disabled,omitempty"`
}

func (t Token) expired(now time.Time) bool {
	return t.Expires != 0 && now.Unix() >= t.Expires
}

// tokenHash 用于在上传日志里标记来源，避免把 token 明文写进日志
func tokenHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:6])
}

// Settings 是可以在后台修改的配置，保存在配置目录的 settings.json
type Settings struct {
	MaxMB         int64    `json:"max_mb"`
	Compress      bool     `json:"compress"`
	Quality       int      `json:"quality"`
	Convert       string   `json:"convert"` // "" 不转换，"webp" 转为 WebP
	Watermark     bool     `json:"watermark"`
	WaterText     string   `json:"water_text"`
	WaterPos      int      `json:"water_pos"`  // 九宫格位置 1-9，9 为右下
	WaterSize     int      `json:"water_size"` // 字号占图片短边的百分比
	WaterColor    string   `json:"water_color"`
	WaterOpacity  int      `json:"water_opacity"` // 0-100
	Hotlink       bool     `json:"hotlink"`
	AllowedRefs   []string `json:"allowed_referers"`
	AllowEmptyRef bool     `json:"allow_empty_referer"`
	Tokens        []Token  `json:"tokens"`
	// 后台修改过的登录密码（pbkdf2-sha256 加盐哈希），为空时使用 PICNOOK_PASSWORD
	PasswordHash string `json:"password_hash,omitempty"`
}

func (s Settings) clone() Settings {
	c := s
	c.AllowedRefs = append([]string(nil), s.AllowedRefs...)
	c.Tokens = append([]Token(nil), s.Tokens...)
	return c
}

func (s Settings) needsProcessing() bool {
	return s.Compress || s.Convert == "webp" || (s.Watermark && s.WaterText != "")
}

type settingsStore struct {
	mu   sync.RWMutex
	path string
	s    Settings
}

// loadSettings 读取配置；首次启动时用环境变量里的值初始化，之后以后台保存的为准
func loadSettings(path string, defaults Settings) (*settingsStore, bool, error) {
	st := &settingsStore{path: path, s: defaults}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, true, st.save()
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(b, &st.s); err != nil {
		return nil, false, err
	}
	return st, false, nil
}

func (st *settingsStore) get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.clone()
}

func (st *settingsStore) update(fn func(*Settings) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s.clone()
	if err := fn(&next); err != nil {
		return err
	}
	prev := st.s
	st.s = next
	if err := st.save(); err != nil {
		st.s = prev
		return err
	}
	return nil
}

func (st *settingsStore) save() error {
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), st.path)
}

var (
	errTokenInvalid  = errors.New("Token 错误")
	errTokenExpired  = errors.New("Token 已过期")
	errTokenDisabled = errors.New("Token 已禁用")
)

// findToken 校验 API Token；对所有 token 都做一次常量时间比较，不提前返回
func (a *app) findToken(v string) (Token, error) {
	var found Token
	ok := false
	if v != "" {
		for _, t := range a.store.get().Tokens {
			if subtle.ConstantTimeCompare([]byte(v), []byte(t.Value)) == 1 {
				found, ok = t, true
			}
		}
	}
	switch {
	case !ok:
		return Token{}, errTokenInvalid
	case found.Disabled:
		return found, errTokenDisabled
	case found.expired(time.Now()):
		return found, errTokenExpired
	}
	return found, nil
}

// uploadEntry 是上传日志的一行，用来统计每个 token 的上传数量和文件
type uploadEntry struct {
	Time  int64  `json:"t"`
	Path  string `json:"path"`
	Token string `json:"tk,omitempty"` // tokenHash，网页上传为空
	ID    int    `json:"id,omitempty"`
	IP    string `json:"ip"`
	Size  int64  `json:"size"`
}

func (a *app) logUpload(e uploadEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.logMu.Lock()
	defer a.logMu.Unlock()
	f, err := os.OpenFile(a.uploadLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

func (a *app) uploadLogPath() string {
	return filepath.Join(a.cfg.configDir, "uploads.log")
}

func (a *app) readUploadLog() []uploadEntry {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	f, err := os.Open(a.uploadLogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []uploadEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e uploadEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
