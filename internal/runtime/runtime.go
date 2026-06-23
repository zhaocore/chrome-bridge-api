package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Paths struct {
	Home       string
	InstallDir string
	BinDir     string
	PIDFile    string
	LogFile    string
	PrevLog    string
}

type Status struct {
	Running            bool   `json:"running"`
	Port               int    `json:"port"`
	Version            string `json:"version"`
	ExtensionConnected bool   `json:"extension_connected"`
	ExtensionID        string `json:"extension_id"`
	ExtensionVersion   string `json:"extension_version"`
	UptimeSeconds      int    `json:"uptime_seconds"`
}

type LogOptions struct {
	Lines    int
	Follow   bool
	Previous bool
}

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	install := filepath.Join(home, ".chrome-bridge")
	return Paths{
		Home:       home,
		InstallDir: install,
		BinDir:     filepath.Join(install, "bin"),
		PIDFile:    filepath.Join(install, "chrome-bridge.pid"),
		LogFile:    filepath.Join(install, "daemon.log"),
		PrevLog:    filepath.Join(install, "daemon.prev.log"),
	}
}

func EnsureDirs(paths Paths) error {
	if err := os.MkdirAll(paths.InstallDir, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(paths.BinDir, 0o755)
}

func WritePID(paths Paths, pid int) error {
	if err := EnsureDirs(paths); err != nil {
		return err
	}
	return os.WriteFile(paths.PIDFile, []byte(strconv.Itoa(pid)), 0o644)
}

func ReadPID(paths Paths) (int, error) {
	raw, err := os.ReadFile(paths.PIDFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func RemovePID(paths Paths) {
	_ = os.Remove(paths.PIDFile)
}

func BaseURL(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func Start(paths Paths, executable string, port int) error {
	if err := EnsureDirs(paths); err != nil {
		return err
	}
	if status, err := FetchStatus(BaseURL("127.0.0.1", port), 500*time.Millisecond, port); err == nil && status.Running {
		return nil
	}
	if _, err := os.Stat(paths.LogFile); err == nil {
		_ = os.Rename(paths.LogFile, paths.PrevLog)
	}
	logFile, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(executable, "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "CHROME_BRIDGE_DAEMON=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := WritePID(paths, cmd.Process.Pid); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func Stop(paths Paths) error {
	pid, err := ReadPID(paths)
	if err != nil {
		return err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	RemovePID(paths)
	return nil
}

func FetchStatus(baseURL string, timeout time.Duration, fallbackPort int) (Status, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/status")
	if err != nil {
		return Status{Running: false, Port: fallbackPort}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Status{Running: false, Port: fallbackPort}, fmt.Errorf("status returned HTTP %d", resp.StatusCode)
	}
	var status Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return Status{Running: false, Port: fallbackPort}, err
	}
	status.Running = true
	return status, nil
}

func ParseLogOptions(args []string) (LogOptions, error) {
	opts := LogOptions{Lines: 100}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n":
			if i+1 >= len(args) {
				return opts, errors.New("logs: -n requires a number")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return opts, errors.New("logs: -n must be a positive integer")
			}
			opts.Lines = n
			i++
		case "-f":
			opts.Follow = true
		case "--prev":
			opts.Previous = true
		default:
			return opts, fmt.Errorf("logs: unknown option %s", args[i])
		}
	}
	return opts, nil
}

func PrintLogs(paths Paths, opts LogOptions, out io.Writer) error {
	path := paths.LogFile
	if opts.Previous {
		path = paths.PrevLog
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	start := 0
	if len(lines) > opts.Lines {
		start = len(lines) - opts.Lines
	}
	for _, line := range lines[start:] {
		if line != "" {
			fmt.Fprintln(out, line)
		}
	}
	if opts.Follow {
		return followFile(path, out)
	}
	return nil
}

func followFile(path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func InstallSkill(repoRoot string, paths Paths) error {
	src := filepath.Join(repoRoot, "chrome-bridge-skill")
	if _, err := os.Stat(src); err != nil {
		return err
	}
	dst := filepath.Join(paths.InstallDir, "skills", "chrome-bridge")
	_ = os.RemoveAll(dst)
	return copyDir(src, dst)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
