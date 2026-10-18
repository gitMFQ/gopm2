package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// AuthStore 抽象认证持久化。
type AuthStore interface {
	SetAuth(key string, value []byte) error
	GetAuth(key string) ([]byte, error)
}

// Auth 负责密码、session 与 bearer token。
type Auth struct {
	store AuthStore

	mu       sync.Mutex
	sessions map[string]time.Time // token -> 过期时间
}

const (
	authKeyPassword = "password_hash"
	sessionCookie   = "gopm2_session"
	sessionTTL      = 24 * time.Hour
)

// NewAuth 创建认证组件。
func NewAuth(store AuthStore) *Auth {
	return &Auth{store: store, sessions: make(map[string]time.Time)}
}

// HasPassword 判断管理员密码是否已初始化。
func (a *Auth) HasPassword() bool {
	v, _ := a.store.GetAuth(authKeyPassword)
	return len(v) > 0
}

// InitPassword 生成随机密码并保存 bcrypt 哈希，返回明文密码（仅此一次）。
func (a *Auth) InitPassword() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	password := hex.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if err := a.store.SetAuth(authKeyPassword, hash); err != nil {
		return "", err
	}
	return password, nil
}

// CheckPassword 校验密码。
func (a *Auth) CheckPassword(password string) bool {
	hash, _ := a.store.GetAuth(authKeyPassword)
	if len(hash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

// NewSession 创建 session，返回 token。
func (a *Auth) NewSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(sessionTTL)
	// 顺手清理过期 session
	for k, exp := range a.sessions {
		if time.Now().After(exp) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	return tok, nil
}

// DestroySession 销毁 session。
func (a *Auth) DestroySession(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

func (a *Auth) validSession(tok string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, tok)
		return false
	}
	return true
}

// Middleware 要求 session cookie 或 Bearer token。
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && a.validSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		if len(h) > 7 && h[:7] == "Bearer " {
			tok := h[7:]
			if a.validSession(tok) {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, errString("unauthorized"))
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.auth.CheckPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, errString("wrong password"))
		return
	}
	tok, err := s.auth.NewSession()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.DestroySession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
