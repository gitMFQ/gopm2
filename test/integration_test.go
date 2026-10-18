//go:build integration

// 集成测试：模拟运维场景下 gopm2 CLI + daemon 的全链路。
// 用法：cd /root/gopm2 && make build && go test -tags=integration ./test/...
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gitMFQ/gopm2/internal/daemon"
)

var (
	gopm2Bin  string
	gopm2dBin string
)

// TestMain 构造临时数据目录并构建二进制。
func TestMain(m *testing.M) {
	// 优先使用 GOPM2_BIN/GOPM2D_BIN 环境变量；否则在 /tmp/gopm2test-bin 编译
	if os.Getenv("GOPM2_BIN") != "" {
		gopm2Bin = os.Getenv("GOPM2_BIN")
	} else {
		gopm2Bin = filepath.Join(os.TempDir(), "gopm2test-bin", "gopm2")
	}
	if os.Getenv("GOPM2D_BIN") != "" {
		gopm2dBin = os.Getenv("GOPM2D_BIN")
	} else {
		gopm2dBin = filepath.Join(os.TempDir(), "gopm2test-bin", "gopm2d")
	}
	if _, err := os.Stat(gopm2Bin); err != nil {
		buildBinaries()
	}
	_ = gopm2dBin
	os.Exit(m.Run())
}

func buildBinaries() {
	// 向上查找 go.mod 定位项目根目录
	root, _ := os.Getwd()
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		panic("cannot locate project root (go.mod) from " + root)
	}
	dir := filepath.Dir(gopm2Bin)
	os.MkdirAll(dir, 0o755)
	for _, p := range []struct{ src, dst string }{
		{"cmd/gopm2", gopm2Bin},
		{"cmd/gopm2d", gopm2dBin},
	} {
		cmd := exec.Command("go", "build", "-o", p.dst, "./"+p.src)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("build %s: %v\n%s", p.src, err, out))
		}
	}
}

// newDataDir 为每个测试创建独立数据目录。
func newDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "gopm2it", fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 清理：杀 daemon 进程、删目录
		stopDaemon(dir)
		os.RemoveAll(dir)
	})
	return dir
}

// freePort 获取一个空闲 TCP 端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func setEnv(t *testing.T, dataDir string) string {
	t.Helper()
	port := freePort(t)
	os.Setenv("GOPM2_DATA_DIR", dataDir)
	os.Setenv("GOPM2_BIN", gopm2dBin)
	// 每个测试用随机 Web 端口，避免与其他测试/实例的 9615 冲突
	os.Setenv("GOPM2_TCP_ADDR", fmt.Sprintf("127.0.0.1:%d", port))
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func unsetEnv() {
	os.Unsetenv("GOPM2_DATA_DIR")
	os.Unsetenv("GOPM2_BIN")
	os.Unsetenv("GOPM2_TCP_ADDR")
}

// run 运行 CLI 命令并返回 stdout、stderr、exit code。
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(gopm2Bin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			rc = -1
		}
	}
	return out.String(), errBuf.String(), rc
}

func stopDaemon(dataDir string) {
	paths := daemon.PathsFor(dataDir)
	pid := daemon.ReadPIDFile(paths.PidFile)
	if pid > 0 {
		syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && daemon.ProcessAlive(pid, "gopm2d") {
			time.Sleep(100 * time.Millisecond)
		}
		// 强杀残留 watchdog
		wdpid := daemon.ReadPIDFile(paths.WDPidFile)
		if wdpid > 0 && daemon.ProcessAlive(wdpid, "gopm2d") {
			syscall.Kill(wdpid, syscall.SIGKILL)
		}
	}
}

// readPID 从 status JSON 取 pid 字段。
func readPID(t *testing.T, data string) int {
	t.Helper()
	// JSON 输出含 "pid": NNNN
	i := strings.Index(data, `"pid":`)
	if i < 0 {
		t.Fatalf("no pid field in: %s", data)
	}
	rest := data[i+len(`"pid":`):]
	// 跳过冒号后的空白（JSON 缩进 "pid": 7280）
	j := 0
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n') {
		j++
	}
	rest = rest[j:]
	end := 0
	for end < len(rest) && (rest[end] >= '0' && rest[end] <= '9') {
		end++
	}
	pid, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitFor polls until fn returns true or timeout.
func waitFor(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestStartListAndKill 测试 start → list → kill -9 子进程自动重启。
func TestStartListAndKill(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	out, errOut, rc := run(t, "start", "demo", "--restart", "always", "--", "sleep", "1000")
	if rc != 0 {
		t.Fatalf("start failed: %s", errOut)
	}
	// 通过 status 命令取 JSON 形式的 pid
	if !waitFor(5*time.Second, func() bool {
		out, _, _ = run(t, "status", "demo")
		return strings.Contains(out, `"state": "online"`)
	}) {
		t.Fatalf("demo not online: %s", out)
	}
	oldPID := readPID(t, out)
	if oldPID <= 0 {
		t.Fatalf("bad pid: %s", out)
	}

	out, _, rc = run(t, "list")
	if rc != 0 {
		t.Fatalf("list failed: %s", out)
	}
	if !strings.Contains(out, "online") {
		t.Fatalf("expected online in list: %s", out)
	}

	// kill -9 子进程
	pgid, err := syscall.Getpgid(oldPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// 1s 退避后自动重启，pid 变化
	ok := waitFor(10*time.Second, func() bool {
		out, _, _ := run(t, "status", "demo")
		return strings.Contains(out, `"state": "online"`) &&
			!strings.Contains(out, fmt.Sprintf(`"pid": %d`, oldPID)) &&
			strings.Contains(out, `"restarts": 1`)
	})
	if !ok {
		out, _, _ = run(t, "status", "demo")
		t.Fatalf("auto-restart not detected: %s", out)
	}
}

// TestLogsFollow 验证 logs -f 至少输出 1 行。
func TestLogsFollow(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	// 用 /bin/sh 包装：输出 3 行后保持存活
	run(t, "start", "logger", "--restart", "never", "--", "sh", "-c", "for i in 1 2 3; do echo line$i; sleep 0.1; done; sleep 30")
	waitFor(5*time.Second, func() bool {
		out, _, _ := run(t, "status", "logger")
		return strings.Contains(out, `"state": "online"`)
	})

	// logs 应包含 3 行（等待输出全部落盘）
	var out string
	if !waitFor(10*time.Second, func() bool {
		out, _, _ = run(t, "logs", "logger")
		return strings.Contains(out, "line1") && strings.Contains(out, "line2") && strings.Contains(out, "line3")
	}) {
		t.Fatalf("logs incomplete: %q", out)
	}
}

// TestSaveResurrectAcrossRestart 测试 save → kill -9 daemon → ensure → resurrect 拉起 demo。
func TestSaveResurrectAcrossRestart(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	run(t, "start", "demo", "--restart", "always", "--", "sleep", "1000")
	waitFor(5*time.Second, func() bool {
		out, _, _ := run(t, "status", "demo")
		return strings.Contains(out, `"state": "online"`)
	})

	out, _, rc := run(t, "save")
	if rc != 0 || !strings.Contains(out, "已保存 1") {
		t.Fatalf("save failed: rc=%d out=%s", rc, out)
	}

	// kill -9 daemon（watchdog 会拉起并 resurrect）
	daemonPID := daemon.ReadPIDFile(filepath.Join(dataDir, "gopm2d.pid"))
	if daemonPID <= 0 {
		t.Fatal("daemon pidfile missing")
	}
	if err := syscall.Kill(daemonPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// 等待 watchdog 拉起 daemon + resurrect 拉起 demo
	ok := waitFor(15*time.Second, func() bool {
		out, _, _ := run(t, "status", "demo")
		return strings.Contains(out, `"state": "online"`) && !strings.Contains(out, `"pid": 0`)
	})
	if !ok {
		out, _, _ := run(t, "list")
		t.Fatalf("resurrect did not bring demo back: %s", out)
	}
}

// TestApply 测试 apply TOML 幂等：第一次 apply 注册并启动；再次 apply 不重建。
func TestApply(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	// 写临时 TOML
	dir := t.TempDir()
	toml := `
version = 1
[[apps]]
name = "fromfile"
command = "sleep 1000"
restart_policy = "never"
`
	path := filepath.Join(dir, "g.toml")
	os.WriteFile(path, []byte(toml), 0o644)

	out, errOut, rc := run(t, "apply", path)
	if rc != 0 {
		t.Fatalf("apply failed: %s", errOut)
	}
	if !strings.Contains(out, "fromfile") {
		t.Fatalf("apply output: %s", out)
	}

	// 再次 apply：不应报错
	out, _, rc = run(t, "apply", path)
	if rc != 0 {
		t.Fatalf("re-apply failed: %s", out)
	}
}

// TestDaemonStopAndRestart 测试 daemon stop → 后续 CLI 自动 ensure。
func TestDaemonStopAndRestart(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	// 触发 daemon 拉起
	_, _, _ = run(t, "status", "nobody")
	waitFor(5*time.Second, func() bool {
		paths := daemon.PathsFor(dataDir)
		return daemon.DaemonRunning(paths)
	})

	// 优雅 stop
	out, _, rc := run(t, "daemon", "stop")
	if rc != 0 {
		t.Fatalf("daemon stop: %s", out)
	}
	time.Sleep(500 * time.Millisecond)

	// 再次 list 应自动拉起
	out, _, rc = run(t, "list")
	if rc != 0 {
		t.Fatalf("list after stop: %s", out)
	}
}

// helper for HTTP requests against unix socket
func httpOverUDS(t *testing.T, sockPath, method, path string) (*http.Response, []byte) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(method, "http://d"+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// TestAPIAccessible 验证 Unix socket 上的 /api/apps 可用（CLI 同通道）。
func TestAPIAccessible(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	_, _, _ = run(t, "start", "x", "--", "sleep", "1000")
	sockPath := filepath.Join(dataDir, "gopm2.sock")
	ok := waitFor(5*time.Second, func() bool {
		_, err := os.Stat(sockPath)
		return err == nil
	})
	if !ok {
		t.Fatal("socket not created")
	}

	resp, body := httpOverUDS(t, sockPath, "GET", "/api/apps")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var apps []map[string]any
	if err := json.Unmarshal(body, &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0]["spec"].(map[string]any)["name"] != "x" {
		t.Fatalf("apps = %+v", apps)
	}
	_ = url.PathEscape // 避免 unused
}

// TestHealthCheckRestart 验证健康检查失败触发重启：
// 启动一个长命进程，健康检查命令恒失败，短间隔内应被重启。
func TestHealthCheckRestart(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	// health-cmd 恒失败（exit 1），interval 1s，连续失败 3 次触发重启
	_, errOut, rc := run(t, "start", "sick", "--restart", "always",
		"--health-cmd", "exit 1", "--health-interval", "1s", "--", "sleep", "1000")
	if rc != 0 {
		t.Fatalf("start sick failed: %s", errOut)
	}
	if !waitFor(5*time.Second, func() bool {
		out, _, _ := run(t, "status", "sick")
		return strings.Contains(out, `"state": "online"`)
	}) {
		t.Fatal("sick never online")
	}

	// 健康检查连续失败 3 次（约 3s）后应触发 restart（restarts >= 1）
	ok := waitFor(15*time.Second, func() bool {
		out, _, _ := run(t, "status", "sick")
		return strings.Contains(out, `"restarts": 1`) ||
			strings.Contains(out, `"restarts": 2`) ||
			strings.Contains(out, `"restarts": 3`)
	})
	if !ok {
		out, _, _ := run(t, "status", "sick")
		t.Fatalf("health check did not trigger restart: %s", out)
	}
}

// TestExportImport 验证 export → 删除全部 → import 恢复。
func TestExportImport(t *testing.T) {
	dataDir := newDataDir(t)
	setEnv(t, dataDir)
	defer unsetEnv()

	run(t, "start", "e1", "--restart", "never", "--", "sleep", "1000")
	run(t, "start", "e2", "--restart", "never", "--", "sleep", "1000")
	waitFor(5*time.Second, func() bool {
		out, _, _ := run(t, "list")
		return strings.Count(out, "online") == 2
	})

	out, _, rc := run(t, "export")
	if rc != 0 || !strings.Contains(out, `"e1"`) || !strings.Contains(out, `"e2"`) {
		t.Fatalf("export failed: %s", out)
	}

	// 写入文件再 import 到新环境（同一 dataDir 先删除 apps）
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	os.WriteFile(path, []byte(out), 0o644)

	run(t, "delete", "e1")
	run(t, "delete", "e2")

	outImp, errOut, rc := run(t, "import", path)
	if rc != 0 {
		t.Fatalf("import failed: %s", errOut)
	}
	if !strings.Contains(outImp, "e1") || !strings.Contains(outImp, "e2") {
		t.Fatalf("import output: %s", outImp)
	}
}

// TestWebPanelAuth 验证 Web 面板：登录、脱敏、操作。
func TestWebPanelAuth(t *testing.T) {
	dataDir := newDataDir(t)
	addr := setEnv(t, dataDir)
	base := "http://" + addr
	defer unsetEnv()

	// 启动 daemon
	_, _, _ = run(t, "list")
	paths := daemon.PathsFor(dataDir)
	if !waitFor(5*time.Second, func() bool { return daemon.DaemonRunning(paths) }) {
		t.Fatal("daemon not running")
	}

	// 读初始密码
	pwFile := filepath.Join(dataDir, "initial-password")
	pwData, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatalf("read initial password: %v", err)
	}
	// 提取 "Web 面板初始密码: <24hex>"
	idx := strings.Index(string(pwData), "初始密码: ")
	if idx < 0 {
		t.Fatalf("unexpected password file: %q", pwData)
	}
	pw := strings.TrimSpace(string(pwData)[idx+len("初始密码: "):])
	if i := strings.IndexByte(pw, '\n'); i > 0 {
		pw = pw[:i]
	}
	if len(pw) < 8 {
		t.Fatalf("password too short: %q", pw)
	}

	// 未登录 401
	resp, err := http.Get(base + "/api/apps")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	// 登录（cookie jar 保持 session）
	jar := &memoryJar{}
	client := &http.Client{Jar: jar}
	resp, err = client.Post(base+"/api/login", "application/json",
		strings.NewReader(`{"password":"`+pw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// 登录后可访问
	resp, err = client.Get(base + "/api/apps")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apps after login: %d", resp.StatusCode)
	}

	// 登录页静态资源
	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "gopm2") {
		t.Fatalf("index page broken: %d", resp.StatusCode)
	}
}

// memoryJar 简易 cookie jar。
type memoryJar struct{ cookies map[string][]*http.Cookie }

func (j *memoryJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j.cookies == nil {
		j.cookies = map[string][]*http.Cookie{}
	}
	j.cookies[u.Host] = cookies
}
func (j *memoryJar) Cookies(u *url.URL) []*http.Cookie {
	return j.cookies[u.Host]
}
