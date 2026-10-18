// gopm2 是 gopm2d 的 CLI 客户端。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gitMFQ/gopm2/internal/client"
	"github.com/gitMFQ/gopm2/internal/config"
	"github.com/gitMFQ/gopm2/internal/daemon"
	"github.com/gitMFQ/gopm2/internal/types"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Args[2:])
	case "stop":
		err = withClientCmd(os.Args[2:], func(c *client.Client, name string) error { return c.Stop(name) })
	case "restart":
		err = withClientCmd(os.Args[2:], func(c *client.Client, name string) error { return c.Restart(name) })
	case "delete":
		err = withClientCmd(os.Args[2:], func(c *client.Client, name string) error { return c.Delete(name) })
	case "list", "ls":
		err = cmdList()
	case "status":
		err = cmdStatus(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "save":
		err = cmdSave()
	case "resurrect":
		err = cmdResurrect()
	case "apply":
		err = cmdApply(os.Args[2:])
	case "export":
		err = cmdExport()
	case "import":
		err = cmdImport(os.Args[2:])
	case "dashboard":
		err = cmdDashboard()
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "install-cron":
		err = cmdInstallCron()
	case "uninstall-cron":
		err = cmdUninstallCron()
	case "version", "--version", "-v":
		fmt.Println("gopm2", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `gopm2 - 轻量级进程管理器 (类 PM2)

用法:
  gopm2 start <name> [flags] -- <command...>   启动并守护进程
  gopm2 stop <name>                            停止
  gopm2 restart <name>                         重启
  gopm2 delete <name>                          删除
  gopm2 list                                   进程列表
  gopm2 status <name>                          进程详情
  gopm2 logs <name> [-f] [--err]               查看/跟踪日志
  gopm2 save                                   保存进程快照
  gopm2 resurrect                              从快照恢复
  gopm2 apply <file.toml> [--prune]            幂等应用 TOML 配置
  gopm2 export / import <file.json>            备份/恢复
  gopm2 dashboard                              打开 Web 面板地址
  gopm2 daemon start|stop|status|ensure        管理 daemon
  gopm2 install-cron / uninstall-cron          安装/卸载 cron 保活

全局环境变量:
  GOPM2_DATA_DIR   数据目录（默认 ~/.gopm2）
  GOPM2_BIN        gopm2d 二进制路径（默认与 gopm2 同目录的 gopm2d）
`)
}

// daemonBin 定位 gopm2d 二进制：优先 GOPM2_BIN，其次与 gopm2 同目录，最后 PATH。
func daemonBin() (string, error) {
	if b := os.Getenv("GOPM2_BIN"); b != "" {
		abs, err := filepath.Abs(b)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("GOPM2_BIN=%s 不存在: %w", abs, err)
		}
		return abs, nil
	}
	self, err := os.Executable()
	if err == nil {
		cand := filepath.Join(filepath.Dir(self), "gopm2d")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("gopm2d"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("找不到 gopm2d 二进制：请确认与 gopm2 同目录，或设置 GOPM2_BIN 环境变量")
}

// newClient 创建 client；autoEnsure=true 时 daemon 不在则自动拉起。
func newClient(autoEnsure bool) (*client.Client, error) {
	paths := daemon.DefaultPaths()
	if autoEnsure && !daemon.DaemonRunning(paths) {
		bin, err := daemonBin()
		if err != nil {
			return nil, err
		}
		if _, err := daemon.EnsureDaemon(paths, bin); err != nil {
			return nil, err
		}
	}
	return client.New(paths), nil
}

// ---- start ----

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "工作目录")
	envs := stringList{}
	fs.Var(&envs, "env", "环境变量 KEY=VALUE（可多次）")
	restart := fs.String("restart", "on-failure", "重启策略 always|on-failure|never")
	maxRestarts := fs.Int("max-restarts", 0, "最大重启次数，0=无限")
	healthCmd := fs.String("health-cmd", "", "健康检查命令")
	healthInterval := fs.String("health-interval", "10s", "健康检查间隔")
	noStart := fs.Bool("no-start", false, "只注册不启动")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: gopm2 start <name> [--cwd dir] [--env K=V]... [--restart p] [--max-restarts n] [--health-cmd cmd] [--health-interval d] -- <command...>\n")
	}
	// flag 包遇第一个非 flag 参数停止解析；手动找 "--"
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	var preArgs, cmdArgs []string
	if sep >= 0 {
		preArgs = args[:sep]
		cmdArgs = args[sep+1:]
	} else {
		// 无 -- 时：name 之后所有参数视为命令（name 见下方提取）
		preArgs = args
	}
	// 手动提取 name（第一个不以 - 开头的参数），其余交给 flag 解析
	var flagArgs []string
	name := ""
	for i := 0; i < len(preArgs); i++ {
		a := preArgs[i]
		if name == "" && !strings.HasPrefix(a, "-") {
			// 需要排除"flag 的值"：检查前一个参数是不是需要值的 flag
			if i > 0 && flagNeedsValue(preArgs[i-1]) {
				flagArgs = append(flagArgs, a)
				continue
			}
			name = a
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	if sep < 0 && name != "" {
		// 无 --：name 之后（原始位置起）的非 flag 参数是命令
		idx := -1
		for i, a := range preArgs {
			if a == name {
				idx = i
				break
			}
		}
		if idx >= 0 {
			for _, a := range preArgs[idx+1:] {
				if strings.HasPrefix(a, "-") {
					break
				}
				cmdArgs = append(cmdArgs, a)
			}
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if name == "" {
		fs.Usage()
		return fmt.Errorf("缺少 name")
	}
	if len(cmdArgs) == 0 {
		return fmt.Errorf("缺少命令，用法: gopm2 start %s -- <command...>", name)
	}

	spec := types.Spec{
		Name:        name,
		Command:     cmdArgs[0],
		Args:        cmdArgs[1:],
		Cwd:         *cwd,
		Env:         envs.toMap(),
		Restart:     types.RestartPolicy(*restart),
		MaxRestarts: *maxRestarts,
	}
	if *healthCmd != "" {
		d, err := time.ParseDuration(*healthInterval)
		if err != nil {
			return fmt.Errorf("--health-interval: %w", err)
		}
		spec.Health = types.HealthSpec{Type: types.HealthExec, Target: *healthCmd, Interval: types.Duration(d)}
	}
	if err := spec.Normalize(); err != nil {
		return err
	}

	c, err := newClient(true)
	if err != nil {
		return err
	}
	app, err := c.CreateApp(spec, !*noStart)
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s 已注册 (state=%s pid=%d)\n", app.Spec.Name, app.Status.State, app.Status.PID)
	return nil
}

// flagNeedsValue 判断 flag 名是否需要跟随一个值（用于参数提取）。
func flagNeedsValue(a string) bool {
	if strings.Contains(a, "=") {
		return false
	}
	switch strings.TrimLeft(a, "-") {
	case "cwd", "env", "restart", "max-restarts", "health-cmd", "health-interval":
		return true
	}
	return false
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
func (s *stringList) toMap() map[string]string {
	m := map[string]string{}
	for _, kv := range *s {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ---- 通用单名命令 ----

func withClientCmd(args []string, fn func(*client.Client, string) error) error {
	if len(args) < 1 {
		return fmt.Errorf("缺少 name")
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	if err := fn(c, args[0]); err != nil {
		return err
	}
	fmt.Printf("✓ %s 完成\n", args[0])
	return nil
}

// ---- list / status ----

func cmdList() error {
	c, err := newClient(true)
	if err != nil {
		return err
	}
	apps, err := c.List()
	if err != nil {
		return err
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Spec.Name < apps[j].Spec.Name })
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tPID\tCPU%\tRSS\tRESTARTS\tUPTIME")
	for _, a := range apps {
		up := ""
		if a.Status.State == types.StateOnline && !a.Status.StartedAt.IsZero() {
			up = time.Since(a.Status.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f\t%s\t%d\t%s\n",
			a.Spec.Name, a.Status.State, a.Status.PID,
			a.Status.CPUPercent, humanBytes(a.Status.RSSBytes),
			a.Status.Restarts, up)
	}
	tw.Flush()
	return nil
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0"
	}
	units := []string{"B", "KB", "MB", "GB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

func cmdStatus(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("缺少 name")
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	a, err := c.Get(args[0])
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(a, "", "  ")
	fmt.Println(string(data))
	return nil
}

// ---- logs ----

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "持续跟踪")
	stream := fs.String("stream", "out", "out|err")
	errFlag := fs.Bool("err", false, "等同于 --stream=err")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: gopm2 logs <name> [-f] [--err] [--stream out|err]\n")
	}
	// Go flag 包遇第一个非 flag 参数即停止解析，而习惯用法是 `logs <name> -f`，
	// 因此先把 name 摘出来，其余参数再交给 flag 解析。
	var flagArgs []string
	name := ""
	for _, a := range args {
		if name == "" && !strings.HasPrefix(a, "-") {
			name = a
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if name == "" {
		if rest := fs.Args(); len(rest) > 0 {
			name = rest[0]
		}
	}
	if name == "" {
		fs.Usage()
		return fmt.Errorf("缺少 name")
	}
	if *errFlag {
		*stream = "err"
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	if !*follow {
		data, err := c.LogsTail(name, *stream)
		if err != nil {
			return err
		}
		os.Stdout.Write(data)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()
	return c.FollowLogs(ctx, name, *stream, os.Stdout)
}

// ---- save / resurrect / apply / export / import ----

func cmdSave() error {
	c, err := newClient(true)
	if err != nil {
		return err
	}
	n, err := c.Save()
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已保存 %d 个应用快照\n", n)
	return nil
}

func cmdResurrect() error {
	c, err := newClient(true)
	if err != nil {
		return err
	}
	started, errs, err := c.Resurrect()
	if err != nil {
		return err
	}
	fmt.Printf("✓ 恢复 %d 个应用: %s\n", len(started), strings.Join(started, ", "))
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "  ! %s\n", e)
	}
	return nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	prune := fs.Bool("prune", false, "删除配置中不存在的应用")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: gopm2 apply <file.toml> [--prune]")
	}
	specs, err := config.LoadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	changed, err := c.Apply(specs, *prune)
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已应用: %s\n", strings.Join(changed, ", "))
	return nil
}

func cmdExport() error {
	c, err := newClient(true)
	if err != nil {
		return err
	}
	specs, err := c.Export()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func cmdImport(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: gopm2 import <file.json>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var specs []*types.Spec
	if err := json.Unmarshal(data, &specs); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	changed, err := c.Import(specs)
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已导入: %s\n", strings.Join(changed, ", "))
	return nil
}

// ---- dashboard / daemon / cron ----

func cmdDashboard() error {
	addr := os.Getenv("GOPM2_TCP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9615"
	}
	url := "http://" + addr
	fmt.Printf("Web 面板: %s\n", url)
	// 尽力打开浏览器（无 GUI 环境静默失败）
	for _, bin := range []string{"xdg-open", "x-www-browser", "open"} {
		if _, err := exec.LookPath(bin); err == nil {
			cmd := exec.Command(bin, url)
			cmd.Stdout = nil
			cmd.Stderr = nil
			if cmd.Start() == nil {
				fmt.Println("（已在浏览器打开；若没有弹出请手动访问上面地址）")
				return nil
			}
		}
	}
	fmt.Println("（未找到浏览器，请手动访问上面地址）")
	return nil
}

func cmdDaemon(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: gopm2 daemon start|stop|status|ensure")
	}
	paths := daemon.DefaultPaths()
	switch args[0] {
	case "start", "ensure":
		bin, err := daemonBin()
		if err != nil {
			return err
		}
		spawned, err := daemon.EnsureDaemon(paths, bin)
		if err != nil {
			return err
		}
		if spawned {
			fmt.Println("✓ daemon 已拉起 (pid:", daemon.ReadPIDFile(paths.PidFile), ")")
		} else {
			fmt.Println("✓ daemon 已在运行 (pid:", daemon.ReadPIDFile(paths.PidFile), ")")
		}
	case "stop":
		if err := daemon.StopDaemon(paths, 10*time.Second); err != nil {
			return err
		}
		fmt.Println("✓ daemon 已停止")
	case "status":
		pid := daemon.ReadPIDFile(paths.PidFile)
		if daemon.DaemonRunning(paths) {
			fmt.Printf("running (pid %d, socket %s)\n", pid, paths.Socket)
		} else {
			fmt.Println("stopped")
		}
	default:
		return fmt.Errorf("unknown daemon subcommand %q", args[0])
	}
	return nil
}

func cmdInstallCron() error {
	fmt.Println("install-cron 将在 M5 实现")
	return nil
}

func cmdUninstallCron() error {
	fmt.Println("uninstall-cron 将在 M5 实现")
	return nil
}
