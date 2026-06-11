package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBackendPort  = 7860
	defaultFrontendPort = 5174
	defaultAdminKey     = "admin"
)

type commandSpec struct {
	name string
	dir  string
	env  []string
	exe  string
	args []string
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}

	backendPort := flag.Int("port", envInt("PORT", defaultBackendPort), "Go backend port")
	frontendPort := flag.Int("frontend-port", envInt("FRONTEND_PORT", defaultFrontendPort), "Vite frontend port")
	adminKey := flag.String("admin-key", envString("ADMIN_KEY", defaultAdminKey), "admin key")
	installBrowsers := flag.Bool("install-browsers", false, "install browser runtime before starting")
	skipNPMInstall := flag.Bool("skip-npm-install", false, "skip automatic frontend npm install")
	flag.Parse()
	requestedPrewarmTarget := maxInt(envInt("CHAT_ID_PREWARM_TARGET_PER_ACCOUNT", 0), 0)

	if freePort, changed := firstFreePort(*backendPort); changed {
		fmt.Printf("[launcher] backend port %d is busy, using %d\n", *backendPort, freePort)
		*backendPort = freePort
	}
	if freePort, changed := firstFreePort(*frontendPort); changed {
		fmt.Printf("[launcher] frontend port %d is busy, using %d\n", *frontendPort, freePort)
		*frontendPort = freePort
	}

	backendDir := filepath.Join(root, "backend")
	frontendDir := filepath.Join(root, "frontend")
	mustDir(backendDir)
	mustDir(frontendDir)

	goExe, err := findGo()
	if err != nil {
		fatal(err)
	}
	nodeExe, err := findNode()
	if err != nil {
		fatal(err)
	}
	npmCLI, err := findNPMCLI(nodeExe)
	if err != nil {
		fatal(err)
	}

	backendExe := filepath.Join(root, "bin", exeName("qwen2api-backend"))
	must(os.MkdirAll(filepath.Dir(backendExe), 0o755))

	fmt.Println("qwen2API launcher")
	fmt.Println("root:     ", root)
	fmt.Println("go:       ", goExe)
	fmt.Println("node:     ", nodeExe)
	fmt.Println("backend:  ", fmt.Sprintf("http://127.0.0.1:%d", *backendPort))
	fmt.Println("frontend: ", fmt.Sprintf("http://127.0.0.1:%d", *frontendPort))
	fmt.Println()

	runForeground(commandSpec{
		name: "build-backend",
		dir:  backendDir,
		exe:  goExe,
		args: []string{"build", "-o", backendExe, "."},
	})

	if *installBrowsers {
		runForeground(commandSpec{
			name: "install-browsers",
			dir:  root,
			env:  backendEnv(root, *backendPort, *adminKey, 0),
			exe:  backendExe,
			args: []string{"--install-browsers"},
		})
	}

	viteJS := filepath.Join(frontendDir, "node_modules", "vite", "bin", "vite.js")
	if !fileExists(viteJS) && !*skipNPMInstall {
		runForeground(commandSpec{
			name: "npm-install",
			dir:  frontendDir,
			exe:  nodeExe,
			args: []string{npmCLI, "install"},
		})
	}
	if !fileExists(viteJS) {
		fatalf("frontend dependency not found: %s\nRun without --skip-npm-install, or install frontend dependencies first.", viteJS)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendCmd := startBackground(ctx, commandSpec{
		name: "backend",
		dir:  root,
		env:  backendEnv(root, *backendPort, *adminKey, 0),
		exe:  backendExe,
	})

	if err := waitHTTP(fmt.Sprintf("http://127.0.0.1:%d/healthz", *backendPort), 20*time.Second); err != nil {
		fmt.Println("[launcher] backend health check warning:", err)
	}

	frontendEnv := []string{
		"VITE_BACKEND_PROXY_TARGET=http://localhost:" + strconv.Itoa(*backendPort),
	}
	if *adminKey == defaultAdminKey {
		frontendEnv = append(frontendEnv, "VITE_DEFAULT_ADMIN_KEY="+defaultAdminKey)
	}

	frontendCmd := startBackground(ctx, commandSpec{
		name: "frontend",
		dir:  frontendDir,
		env:  frontendEnv,
		exe:  nodeExe,
		args: []string{
			viteJS,
			"--host", "0.0.0.0",
			"--port", strconv.Itoa(*frontendPort),
		},
	})

	if err := waitHTTP(fmt.Sprintf("http://127.0.0.1:%d/", *frontendPort), 30*time.Second); err != nil {
		fmt.Println("[launcher] frontend health check warning:", err)
	}

	fmt.Println()
	fmt.Println("started")
	fmt.Printf("backend:  http://127.0.0.1:%d\n", *backendPort)
	fmt.Printf("frontend: http://127.0.0.1:%d\n", *frontendPort)
	fmt.Println("press Ctrl+C to stop both services")
	fmt.Println()

	if requestedPrewarmTarget > 0 {
		if err := enablePrewarmAfterStart(*backendPort, *adminKey, requestedPrewarmTarget); err != nil {
			fmt.Println("[launcher] Chat_ID prewarm enable warning:", err)
		}
	}

	waitForStop(cancel, backendCmd, frontendCmd)
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func findGo() (string, error) {
	if p, err := exec.LookPath("go.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	fallback := `D:\go\bin\go.exe`
	if fileExists(fallback) {
		return fallback, nil
	}
	return "", errors.New("go.exe not found in PATH or D:\\go\\bin\\go.exe")
}

func findNode() (string, error) {
	if p, err := exec.LookPath("node.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("node"); err == nil {
		return p, nil
	}
	return "", errors.New("node.exe not found in PATH; install Node.js to start the frontend")
}

func findNPMCLI(nodeExe string) (string, error) {
	candidates := []string{}
	nodeDir := filepath.Dir(nodeExe)
	candidates = append(candidates,
		filepath.Join(nodeDir, "node_modules", "npm", "bin", "npm-cli.js"),
		filepath.Join(filepath.Dir(nodeDir), "node_modules", "npm", "bin", "npm-cli.js"),
		// Homebrew on macOS / Linux: npm ships under libexec/lib or lib/.
		// nodeDir is typically .../Cellar/node/<ver>/bin, so the parent is the keg root.
		filepath.Join(nodeDir, "..", "libexec", "lib", "node_modules", "npm", "bin", "npm-cli.js"),
		filepath.Join(nodeDir, "..", "lib", "node_modules", "npm", "bin", "npm-cli.js"),
	)
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "npm", "node_modules", "npm", "bin", "npm-cli.js"))
	}
	if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
		candidates = append(candidates, filepath.Join(programFiles, "nodejs", "node_modules", "npm", "bin", "npm-cli.js"))
	}
	if programFilesX86 := os.Getenv("ProgramFiles(x86)"); programFilesX86 != "" {
		candidates = append(candidates, filepath.Join(programFilesX86, "nodejs", "node_modules", "npm", "bin", "npm-cli.js"))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", errors.New("npm-cli.js not found; install Node.js with npm to install frontend dependencies")
}

func backendEnv(root string, port int, adminKey string, prewarmTarget int) []string {
	return []string{
		"BASE_DIR=" + root,
		"PORT=" + strconv.Itoa(port),
		"ADMIN_KEY=" + adminKey,
		"CHAT_ID_PREWARM_TARGET_PER_ACCOUNT=" + strconv.Itoa(maxInt(prewarmTarget, 0)),
	}
}

func enablePrewarmAfterStart(port int, adminKey string, target int) error {
	payload, err := json.Marshal(map[string]int{"chat_id_pool_target": target})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/admin/settings", port)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Printf("[launcher] Chat_ID prewarm enabled after startup: target_per_account=%d\n", target)
	return nil
}

func runForeground(spec commandSpec) {
	fmt.Println("[launcher] running", spec.name)
	cmd := exec.Command(spec.exe, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = mergedEnv(spec.env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("%s failed: %v", spec.name, err)
	}
}

func startBackground(ctx context.Context, spec commandSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, spec.exe, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = mergedEnv(spec.env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatalf("%s stdout pipe failed: %v", spec.name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fatalf("%s stderr pipe failed: %v", spec.name, err)
	}

	if err := cmd.Start(); err != nil {
		fatalf("%s start failed: %v", spec.name, err)
	}

	go prefixOutput(spec.name, stdout)
	go prefixOutput(spec.name, stderr)
	return cmd
}

func prefixOutput(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		fmt.Printf("[%s] %s\n", name, scanner.Text())
	}
}

func waitForStop(cancel context.CancelFunc, cmds ...*exec.Cmd) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	exited := make(chan string, len(cmds))
	var wg sync.WaitGroup
	for _, cmd := range cmds {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		name := filepath.Base(cmd.Path)
		wg.Add(1)
		go func(c *exec.Cmd, n string) {
			defer wg.Done()
			if err := c.Wait(); err != nil {
				exited <- fmt.Sprintf("%s exited: %v", n, err)
				return
			}
			exited <- fmt.Sprintf("%s stopped", n)
		}(cmd, name)
	}

	select {
	case sig := <-signals:
		fmt.Println("[launcher] stop requested:", sig)
	case msg := <-exited:
		fmt.Println("[launcher]", msg)
	}

	cancel()
	for _, cmd := range cmds {
		killProcess(cmd)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Println("[launcher] forced shutdown timeout reached")
	}
}

func killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func waitHTTP(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func firstFreePort(start int) (int, bool) {
	for port := start; port < start+100; port++ {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, port != start
	}
	fatalf("no free port found from %d to %d", start, start+99)
	return start, false
}

func mergedEnv(extra []string) []string {
	env := os.Environ()
	keys := map[string]int{}
	for i, item := range env {
		if k, _, ok := strings.Cut(item, "="); ok {
			keys[strings.ToUpper(k)] = i
		}
	}
	for _, item := range extra {
		k, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(k)
		if idx, exists := keys[upper]; exists {
			env[idx] = item
			continue
		}
		keys[upper] = len(env)
		env = append(env, item)
	}
	return env
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		i, err := strconv.Atoi(v)
		if err == nil {
			return i
		}
	}
	return fallback
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mustDir(path string) {
	info, err := os.Stat(path)
	if err != nil {
		fatal(err)
	}
	if !info.IsDir() {
		fatalf("not a directory: %s", path)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "[launcher] error:", err)
	os.Exit(1)
}

func fatalf(format string, args ...any) {
	fatal(fmt.Errorf(format, args...))
}
