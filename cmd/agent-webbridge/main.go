package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"agent-webbridge-api/internal/runtime"
	"agent-webbridge-api/internal/server"
)

const version = "dev"

// defaultPort is intentionally a string so release builds can override it with:
// go build -ldflags "-X main.defaultPort=10087" ./cmd/agent-webbridge
var defaultPort = "10086"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	paths := runtime.DefaultPaths()
	cmd := os.Args[1]
	port, err := parsePort(defaultPort)
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "serve":
		if err := serve(paths, port); err != nil {
			log.Fatal(err)
		}
	case "start":
		exe, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		if err := runtime.Start(paths, exe, port); err != nil {
			fatal(err)
		}
		fmt.Println("daemon started")
	case "stop":
		if err := runtime.Stop(paths); err != nil {
			fatal(err)
		}
		fmt.Println("daemon stopped")
	case "restart":
		_ = runtime.Stop(paths)
		exe, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		if err := runtime.Start(paths, exe, port); err != nil {
			fatal(err)
		}
		fmt.Println("daemon restarted")
	case "status":
		status, err := runtime.FetchStatus(runtime.BaseURL("127.0.0.1", port), time.Second, port)
		if err != nil {
			status = runtime.Status{Running: false, Port: port}
		}
		_ = json.NewEncoder(os.Stdout).Encode(status)
	case "logs":
		opts, err := runtime.ParseLogOptions(os.Args[2:])
		if err != nil {
			fatal(err)
		}
		if err := runtime.PrintLogs(paths, opts, os.Stdout); err != nil {
			fatal(err)
		}
	case "install-skill":
		repoRoot, err := findRepoRoot()
		if err != nil {
			fatal(err)
		}
		if err := runtime.InstallSkill(repoRoot, paths); err != nil {
			fatal(err)
		}
		fmt.Println("skill installed")
	default:
		usage()
		os.Exit(2)
	}
}

func serve(paths runtime.Paths, port int) error {
	if err := runtime.WritePID(paths, os.Getpid()); err != nil {
		return err
	}
	defer runtime.RemovePID(paths)

	log.Printf("starting agent-webbridge version=%s port=%d", version, port)
	s := server.New(server.Config{Version: version, Port: port}, nil, nil)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Printf("received %s, exiting", sig)
		return nil
	case err := <-errCh:
		return err
	}
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid defaultPort %q: must be 1-65535", value)
	}
	return port, nil
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "agent-chrome-skill", "SKILL.md")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find agent-chrome-skill from %s", wd)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agent-webbridge <start|stop|restart|status|logs|install-skill|serve>")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
