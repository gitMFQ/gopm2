// Package metrics 直接从 /proc 读取进程 CPU 与内存指标。
package metrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sample 是一次指标采样。
type Sample struct {
	CPUPercent float64 // 自上次采样以来的 CPU 使用率（0~100*核数）
	RSSBytes   int64
	Uptime     time.Duration
}

// ProcReader 针对单个 pid 的采样器，内部保存上次 jiffies 以计算 CPU 差值。
type ProcReader struct {
	clkTck  float64
	lastJif uint64
	lastAt  time.Time
}

// NewProcReader 创建采样器。
func NewProcReader() *ProcReader {
	return &ProcReader{clkTck: 100} // Linux 上 USER_HZ 几乎恒为 100；不依赖 sysconf
}

// PidAlive 判断 pid 是否存在且未变成僵尸。
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// 检查 state 字段不是 Z
	if i := lastParen(data); i >= 0 && i+2 < len(data) {
		return data[i+2] != 'Z'
	}
	return true
}

// lastParen 返回 comm 右括号位置（comm 可能含空格/括号，取最后一个 ')'）。
func lastParen(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ')' {
			return i
		}
	}
	return -1
}

// ReadStat 解析 /proc/<pid>/stat，返回 utime+stime jiffies、starttime jiffies、进程状态。
func ReadStat(pid int) (jiffies uint64, startTime uint64, state byte, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, err
	}
	i := lastParen(data)
	if i < 0 || i+2 >= len(data) {
		return 0, 0, 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[i+2:]))
	// fields[0]=state, [11]=utime, [12]=stime, [19]=starttime（相对 ')' 后偏移）
	if len(fields) < 20 {
		return 0, 0, 0, fmt.Errorf("short stat for pid %d", pid)
	}
	state = fields[0][0]
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	start, _ := strconv.ParseUint(fields[19], 10, 64)
	return utime + stime, start, state, nil
}

// ReadRSS 从 /proc/<pid>/status 读取 VmRSS（字节）。
func ReadRSS(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, err := strconv.ParseInt(f[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return kb * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("VmRSS not found for pid %d", pid)
}

var bootTime = readBootTime()

func readBootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Now()
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			sec, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			if sec > 0 {
				return time.Unix(sec, 0)
			}
		}
	}
	return time.Now()
}

// ProcessStartTime 返回进程的启动时间（Wall clock），用于 pid 复用校验。
func ProcessStartTime(pid int) (time.Time, error) {
	_, start, _, err := ReadStat(pid)
	if err != nil {
		return time.Time{}, err
	}
	// start 单位是 jiffies（USER_HZ=100）。先转 Duration 再除，避免整除丢秒内精度。
	return bootTime.Add(time.Duration(start) * time.Second / 100), nil
}

// Sample 采样一次：返回 CPU%（相对上次采样）、RSS、运行时长。
// 首次调用 CPU% 为自进程启动以来的平均值。
func (r *ProcReader) Sample(pid int, startedAt time.Time) (*Sample, error) {
	jif, _, _, err := ReadStat(pid)
	if err != nil {
		return nil, err
	}
	rss, err := ReadRSS(pid)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var cpu float64
	if r.lastAt.IsZero() {
		elapsed := now.Sub(startedAt).Seconds()
		if elapsed > 0 {
			cpu = (float64(jif) / r.clkTck) / elapsed * 100
		}
	} else {
		dj := float64(jif - r.lastJif)
		dt := now.Sub(r.lastAt).Seconds()
		if dt > 0 {
			cpu = (dj / r.clkTck) / dt * 100
		}
	}
	r.lastJif = jif
	r.lastAt = now
	up := time.Duration(0)
	if !startedAt.IsZero() {
		up = now.Sub(startedAt)
	}
	return &Sample{CPUPercent: cpu, RSSBytes: rss, Uptime: up}, nil
}
