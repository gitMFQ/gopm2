// Package api 提供 REST API 与 SSE，同时挂在 Unix socket（CLI）与 TCP（Web）上。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/gitMFQ/gopm2/internal/daemon"
	"github.com/gitMFQ/gopm2/internal/service"
	"github.com/gitMFQ/gopm2/internal/supervisor"
	"github.com/gitMFQ/gopm2/internal/types"
	"github.com/gitMFQ/gopm2/internal/web"
)

// Web 面板静态资源 handler（FileServer 对目录返回 index.html）。
var webStatic = web.StaticHandler()

// Server 组合路由与依赖。
type Server struct {
	svc        *service.Service
	paths      daemon.Paths
	auth       *Auth      // nil 表示不启用认证
	router     chi.Router // TCP（Web 面板）路由，启用认证
	unixRouter chi.Router // Unix socket（CLI）路由，免认证（socket 0600 即访问控制）
	started    time.Time
}

// New 创建 API Server。auth 可为 nil（M2 阶段 Unix socket 免认证）。
func New(svc *service.Service, paths daemon.Paths, auth *Auth) *Server {
	s := &Server{svc: svc, paths: paths, auth: auth, started: time.Now()}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// 注意：不用全局 Timeout——SSE 长连接不能被 60s 掐断；普通 handler 均为短时操作。

	r.Get("/healthz", s.handleHealthz)
	// Web 面板静态资源（登录页无需认证）
	r.Handle("/*", webStatic)
	r.Route("/api", func(r chi.Router) {
		if auth != nil {
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)
		}
		r.Group(func(r chi.Router) {
			if auth != nil {
				r.Use(auth.Middleware)
			}
			r.Get("/apps", s.handleListApps)
			r.Post("/apps", s.handleCreateApp)
			r.Route("/apps/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetApp)
				r.Delete("/", s.handleDeleteApp)
				r.Post("/start", s.handleStartApp)
				r.Post("/stop", s.handleStopApp)
				r.Post("/restart", s.handleRestartApp)
				r.Get("/logs", s.handleLogs)
				r.Get("/metrics", s.handleMetrics)
			})
			r.Post("/save", s.handleSave)
			r.Post("/resurrect", s.handleResurrect)
			r.Post("/apply", s.handleApply)
			// 注意：/export 返回明文 env，仅注册在 Unix socket（CLI）通道，见下方
			r.Post("/import", s.handleImport)
			r.Get("/events", s.handleEvents)
		})
	})
	s.router = r

	// Unix socket 通道（CLI）：socket 文件权限 0600 即访问控制，API 免认证。
	ur := chi.NewRouter()
	ur.Use(middleware.Recoverer)
	ur.Get("/healthz", s.handleHealthz)
	ur.Route("/api", func(r chi.Router) {
		r.Get("/apps", s.handleListApps)
		r.Post("/apps", s.handleCreateApp)
		r.Route("/apps/{name}", func(r chi.Router) {
			r.Get("/", s.handleGetApp)
			r.Delete("/", s.handleDeleteApp)
			r.Post("/start", s.handleStartApp)
			r.Post("/stop", s.handleStopApp)
			r.Post("/restart", s.handleRestartApp)
			r.Get("/logs", s.handleLogs)
			r.Get("/metrics", s.handleMetrics)
		})
		r.Post("/save", s.handleSave)
		r.Post("/resurrect", s.handleResurrect)
		r.Post("/apply", s.handleApply)
		r.Get("/export", s.handleExport) // 明文 env 备份仅限本机 CLI 通道（socket 0600）
		r.Post("/import", s.handleImport)
		r.Get("/events", s.handleEvents)
	})
	s.unixRouter = ur
	return s
}

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler { return s.router }

// ListenUnix 在 Unix socket 上服务（权限 0600）。
func (s *Server) ListenUnix(ctx context.Context) error {
	sockPath := s.paths.Socket
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	os.Chmod(sockPath, 0o600)
	return s.serveWith(ctx, ln, s.unixRouter)
}

// ListenTCP 在 TCP 地址上服务（Web 面板）。
func (s *Server) ListenTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.serve(ctx, ln)
}

// ServeTCP 使用已建立的 listener 服务 Web 面板（调用方先预绑定以便失败时降级）。
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	return s.serveWith(ctx, ln, s.router)
}

func (s *Server) serveWith(ctx context.Context, ln net.Listener, h http.Handler) error {
	httpSrv := &http.Server{Handler: h}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()
	err := httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---- handlers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.started).Seconds(),
	})
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.svc.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 统一脱敏：列表接口同样不得下发明文环境变量；空列表输出 [] 而非 null
	views := make([]*maskedAppView, 0, len(apps))
	for _, a := range apps {
		views = append(views, maskedApp(a))
	}
	writeJSON(w, http.StatusOK, views)
}

type createAppRequest struct {
	Name              string            `json:"name"`
	Command           string            `json:"command"`
	Args              []string          `json:"args,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	Restart           string            `json:"restart,omitempty"`
	MaxRestarts       int               `json:"max_restarts,omitempty"`
	HealthType        string            `json:"health_type,omitempty"`
	HealthTarget      string            `json:"health_target,omitempty"`
	HealthIntervalSec float64           `json:"health_interval_sec,omitempty"`
	AutoStart         bool              `json:"autostart"`
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return
	}
	spec := types.Spec{
		Name:        req.Name,
		Command:     req.Command,
		Args:        req.Args,
		Cwd:         req.Cwd,
		Env:         req.Env,
		Restart:     types.RestartPolicy(req.Restart),
		MaxRestarts: req.MaxRestarts,
	}
	if req.HealthType != "" {
		spec.Health = types.HealthSpec{
			Type:     types.HealthType(req.HealthType),
			Target:   req.HealthTarget,
			Interval: types.Duration(time.Duration(req.HealthIntervalSec * float64(time.Second))),
		}
	}
	app, err := s.svc.AddApp(spec)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.AutoStart {
		if err := s.svc.Start(spec.Name); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if app, err = s.svc.Get(spec.Name); err != nil || app == nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("app disappeared after start"))
			return
		}
	}
	writeJSON(w, http.StatusCreated, maskedApp(app))
}

func (s *Server) appName(r *http.Request) string { return chi.URLParam(r, "name") }

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.Get(s.appName(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app not found"))
		return
	}
	writeJSON(w, http.StatusOK, maskedApp(a))
}

// maskedApp 返回带 env_masked 字段的 App（环境变量脱敏），供 Web 面板使用。
func maskedApp(a *types.App) *maskedAppView {
	v := &maskedAppView{Spec: a.Spec, Status: a.Status}
	v.Spec.Env = nil // 原始 env 不下发
	v.EnvMasked = supervisor.MaskEnv(a.Spec.Env)
	return v
}

// maskedAppView 在 Spec 之外附带脱敏后的环境变量。
type maskedAppView struct {
	Spec      types.Spec        `json:"spec"`
	Status    types.Status      `json:"status"`
	EnvMasked map[string]string `json:"env_masked"`
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Delete(s.appName(r)); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleStartApp(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Start(s.appName(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeAppOrErr(w, s.appName(r))
}

func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Stop(s.appName(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeAppOrErr(w, s.appName(r))
}

func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Restart(s.appName(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeAppOrErr(w, s.appName(r))
}

// writeAppOrErr 返回脱敏后的单个 App。
func (s *Server) writeAppOrErr(w http.ResponseWriter, name string) {
	a, err := s.svc.Get(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app not found"))
		return
	}
	writeJSON(w, http.StatusOK, maskedApp(a))
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.Save()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": n})
}

func (s *Server) handleResurrect(w http.ResponseWriter, r *http.Request) {
	started, errs, err := s.svc.Resurrect()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := map[string]any{"started": started}
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		resp["errors"] = msgs
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	specs, err := s.svc.Export()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, specs)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var specs []*types.Spec
	if err := json.NewDecoder(r.Body).Decode(&specs); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return
	}
	changed, err := s.svc.Import(specs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var specs []*types.Spec
	if err := json.NewDecoder(r.Body).Decode(&specs); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return
	}
	removeMissing := r.URL.Query().Get("remove_missing") == "true"
	changed, err := s.svc.Apply(specs, removeMissing)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}
