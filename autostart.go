package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// run 统一入口：所有外部命令都经 hideConsole 包一层，
// 否则从 GUI 进程调用 taskkill 会闪出控制台黑窗。
func run(name string, args ...string) ([]byte, error) {
	return hideConsole(exec.Command(name, args...)).CombinedOutput()
}

// ServiceExePath 返回后台代理运行时所用映像的位置。
//
// 为什么要单独一份，而不是直接跑用户手上那个 exe：Windows 下正在运行的
// 映像不能删除也不能覆盖。守护进程若跑用户那个文件，那个文件在代理运行期间
// 就一直被占用——用户想删、想换新版、想把它挪个地方，全都做不到，
// 而他对「后台还有个进程攥着它」毫无感知。
//
// 所以约定是：用户手上的 ccproxy.exe 放在任意位置，任何东西都不记住它的路径；
// 需要常驻的那一份由程序自己放进数据目录，自启注册表值也指向它。
//
// 名字刻意不叫 ccproxy.exe。两个同名文件放在一起，看起来像个来路不明的
// 重复副本，升级时哪个是新的还得靠时间戳猜；叫 service 就自解释了。
func ServiceExePath() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	name := "ccproxy-service"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, name), nil
}

// InstallService 把当前可执行文件复制成服务映像。
func InstallService() (string, error) {
	dst, err := ServiceExePath()
	if err != nil {
		return "", err
	}
	src, err := os.Executable()
	if err != nil {
		return "", err
	}
	srcAbs, _ := filepath.Abs(src)
	dstAbs, _ := filepath.Abs(dst)
	if strings.EqualFold(srcAbs, dstAbs) {
		return dst, nil
	}

	data, err := os.ReadFile(srcAbs)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(dstAbs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".ccproxy-service-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmpName, 0o700)
	}
	if err != nil {
		return "", err
	}

	old := dstAbs + oldExeSuffix
	hadOld := false
	if _, err := os.Stat(dstAbs); err == nil {
		_ = os.Remove(old)
		if err := os.Rename(dstAbs, old); err != nil {
			return "", err
		}
		hadOld = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(tmpName, dstAbs); err != nil {
		if hadOld {
			if restoreErr := os.Rename(old, dstAbs); restoreErr != nil {
				return "", fmt.Errorf("install service: %w (restore failed: %v)", err, restoreErr)
			}
		}
		return "", err
	}
	return dstAbs, nil
}

// oldExeSuffix 是升级时旧服务映像的暂存名。
const oldExeSuffix = ".old"

// cleanupInstallLeftovers 删掉上一次升级留下的旧服务映像。
//
// InstallService 只能先改名再写入（Windows 不允许覆盖已加载的映像），于是数据
// 目录里会留下一个 8 MB 的 .old。它只在改名那一刻有用，之后既没人读也没人删。
//
// 放在面板启动时清理，而不是 daemon：daemon 完全可能就是从那个 .old 起来的
// （升级前启动的进程，改名后句柄跟着走）。删不掉就只有这一种情形，
// 忽略即可，下次开面板会再删一次。
func cleanupInstallLeftovers() {
	if exe, err := ServiceExePath(); err == nil {
		_ = os.Remove(exe + oldExeSuffix)
	}
}

// maxLogBytes 是日志文件的体积上限。
const maxLogBytes = 8 << 20

// openLogFile 打开日志，超过上限就从头写。
//
// 一行一请求，长期使用会长到几百 MB。数据目录是 ccproxy 自己的地盘
// （%LOCALAPPDATA%\ccproxy），没人替它管体积，就该自己管。
//
// 刻意不做轮转：多一档就多一个文件，而日志的用途是排查刚刚发生的问题
// （diagnostics 只取最后 20 行），被截掉的那半截没有保留价值。
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	return os.OpenFile(path, flag, 0o600)
}

// EnableAutostart 注册登录时自启。各平台实现见 autostart_windows.go / autostart_other.go。
// 共同约束：绝不要求管理员 / root 权限。
func EnableAutostart(exePath string) error { return enableAutostartOS(exePath) }

func DisableAutostart() error { return disableAutostartOS() }

func IsAutostartEnabled() bool { return isAutostartEnabledOS() }

// SpawnDaemon 立即拉起后台代理，让用户保存配置后无需等到下次登录。
func SpawnDaemon(exePath string) error {
	if runtime.GOOS == "linux" && IsAutostartEnabled() {
		_, err := run("systemctl", "--user", "restart", "ccproxy.service")
		return err
	}
	cmd := hideConsole(exec.Command(exePath, "--daemon"))
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	return cmd.Start()
}

var (
	readStatusForStop      = ReadStatus
	clearStatusForStop     = ClearStatus
	readHealthForStop      = readHealthIdentity
	requestShutdownForStop = requestDaemonShutdown
	waitPortFreeForStop    = waitPortFree
)

func readHealthIdentity(port int, timeout time.Duration) (healthIdentity, error) {
	var h healthIdentity
	resp, err := (&http.Client{Timeout: timeout}).Get(
		fmt.Sprintf("http://127.0.0.1:%d/__ccproxy/health", port))
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("health returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil {
		return h, err
	}
	if !h.OK || h.PID <= 0 || h.Nonce == "" {
		return h, fmt.Errorf("health identity is incomplete")
	}
	return h, nil
}

func daemonIdentityMatchesStatus(port int, timeout time.Duration) error {
	st, fresh := ReadStatus()
	if !fresh || st == nil || st.PID <= 0 || st.Nonce == "" || st.Port != port {
		return fmt.Errorf("no fresh matching daemon status")
	}
	h, err := readHealthIdentity(port, timeout)
	if err != nil {
		return err
	}
	if h.PID != st.PID || h.Nonce != st.Nonce {
		return fmt.Errorf("health identity does not match fresh status")
	}
	return nil
}

func requestDaemonShutdown(port int, nonce string, timeout time.Duration) error {
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/__ccproxy/shutdown", port), nil)
	if err != nil {
		return err
	}
	req.Header.Set(shutdownNonceHeader, nonce)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// StopDaemon only sends an authenticated graceful shutdown after the fresh
// status file and health endpoint prove they identify the same daemon.
func StopDaemon() error {
	if runtime.GOOS == "linux" && IsAutostartEnabled() {
		_, err := run("systemctl", "--user", "stop", "ccproxy.service")
		if err == nil {
			_ = ClearStatus()
		}
		return err
	}
	st, fresh := readStatusForStop()
	if !fresh || st == nil || st.PID <= 0 || st.PID == os.Getpid() {
		return clearStatusForStop()
	}
	if st.Port <= 0 || st.Nonce == "" {
		return fmt.Errorf("拒绝结束 PID %d：状态文件缺少健康身份", st.PID)
	}
	h, err := readHealthForStop(st.Port, 2*time.Second)
	if err != nil {
		return fmt.Errorf("拒绝结束 PID %d：无法验证代理健康身份: %w", st.PID, err)
	}
	if h.PID != st.PID || h.Nonce != st.Nonce {
		return fmt.Errorf("拒绝结束 PID %d：健康身份与状态文件不匹配", st.PID)
	}
	if err := requestShutdownForStop(st.Port, st.Nonce, 2*time.Second); err != nil {
		return fmt.Errorf("无法优雅停止代理进程 (PID %d): %w", st.PID, err)
	}
	if !waitPortFreeForStop(st.Port, 7*time.Second) {
		return fmt.Errorf("代理进程 (PID %d) 已确认停止请求，但端口 %d 未在 7s 内释放", st.PID, st.Port)
	}
	return clearStatusForStop()
}

// waitPortFree 等旧代理真正放开端口。
//
// StopDaemon 返回只代表信号送到，进程退出与端口释放还要一小会儿。
// 原先用固定的 400ms 硬等：短了新进程绑定失败、启动即退出，
// 而 SpawnDaemon 只负责创建进程不负责它活着，界面照样报成功。
//
// 返回 false 说明端口一直被占——多半是 StopDaemon 没杀掉（权限不足），
// 或者被别的程序占了。此时该如实报错，而不是启动一个注定失败的进程。
func waitPortFree(port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitDaemonReady 轮询健康端点，直到代理真正开始服务。
//
// 「进程已创建」不等于「代理可用」：配置有问题、端口被抢，进程会立刻退出。
// 界面上那句「代理已在 127.0.0.1:PORT 运行」必须等这里通过才能说。
func waitDaemonReady(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/__ccproxy/health", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("健康检查返回 HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("后台代理已启动，但 %s 内没有在 127.0.0.1:%d 上开始服务"+
				"（最后一次尝试：%v）。常见原因是该端口被别的程序占用，"+
				"或配置文件无法读取——详情见日志", timeout, port, last)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

var (
	identityMatchesForLateStop = daemonIdentityMatchesStatus
	stopDaemonForLateStop      = StopDaemon
)

// stopLateMatchingDaemon catches a child that becomes ready just after the
// readiness deadline. It never stops a listener without a matching authenticated
// status/health identity.
func stopLateMatchingDaemon(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeTimeout := min(250*time.Millisecond, time.Until(deadline))
		if probeTimeout > 0 && identityMatchesForLateStop(port, probeTimeout) == nil {
			return stopDaemonForLateStop()
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// autostartDefault 决定界面上「开机自启」开关的初始状态。
//
// enabled 是注册表/systemd 的实际状态，installed 表示之前保存过配置。
//
// 从未安装过时默认开启：代理是个后台服务，用户配好之后期待的是「以后一直有效」，
// 而不是每次开机手动跑一次面板。已经装过的一律以实际状态为准——那是用户自己
// 关掉的，默认值没有资格把它覆盖回去。
//
// 抽成具名函数是为了能被守住：写成 `enabled || !installed` 内联在返回值里，
// 日后任何一次「简化」都能悄无声息地改掉这个默认。
func autostartDefault(enabled, installed bool) bool {
	return enabled || !installed
}
