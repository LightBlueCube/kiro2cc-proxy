package agy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// `agy acp --login --gemini-dir DIR` signs an account in without a browser
// on this machine. Verified against agy 1.5.3:
//
//	stdout: Open this link in a browser to log in with Google:
//	          https://accounts.google.com/o/oauth2/auth?...   (PKCE, redirect
//	          https://antigravity.google/oauth-callback)
//	        Then paste the authorization code, or the full URL of the page
//	        you land on, and press Enter:
//	stdin:  the code or the landing URL, one line
//	stdout: Logged in as <email>.                              → exit 0
//	        couldn't log in: token exchange failed: oauth2: ... → exit 1
//
// Without D-Bus the token lands in <gemini-dir>/antigravity-cli/.

const (
	loginURLTimeout    = 30 * time.Second
	loginSubmitTimeout = time.Minute
	loginPrefix        = "Logged in as "
	loginFailPrefix    = "couldn't log in:"
	maxLoginCodeBytes  = 8 << 10
)

// LoginConfig names the account a login signs in.
type LoginConfig struct {
	Bin       string
	Home      string
	GeminiDir string
	Workdir   string
	Env       []string
}

// Login is a running `agy acp --login`.
type Login struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	url   string

	done   chan struct{}
	mu     sync.Mutex
	lines  []string
	result error
	email  string
}

// StartLogin runs the login and waits for its sign-in URL. The caller must
// Submit or Close it.
func StartLogin(ctx context.Context, cfg LoginConfig) (*Login, error) {
	cmd := exec.Command(cfg.Bin, "acp", "--login", "--gemini-dir", cfg.GeminiDir, "--disable-telemetry")
	cmd.Dir = cfg.Workdir
	cmd.Env = append([]string{
		"HOME=" + cfg.Home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"AGY_CLI_DISABLE_AUTO_UPDATE=1",
	}, cfg.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("run %s: %w", cfg.Bin, err)
	}
	writer.Close()

	l := &Login{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	urlFound := make(chan string, 1)
	go l.read(reader, urlFound)

	timer := time.NewTimer(loginURLTimeout)
	defer timer.Stop()
	select {
	case l.url = <-urlFound:
		return l, nil
	case <-l.done:
		return nil, fmt.Errorf("agy login ended before printing a sign-in link: %w", l.Err())
	case <-timer.C:
	case <-ctx.Done():
	}
	l.Close()
	return nil, errors.New("agy login printed no sign-in link")
}

func (l *Login) read(r io.ReadCloser, urlFound chan<- string) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		l.mu.Lock()
		l.lines = append(l.lines, line)
		l.mu.Unlock()
		if signInURL(line) {
			select {
			case urlFound <- line:
			default: // only the first link counts
			}
		}
	}
	waitErr := l.cmd.Wait()
	l.mu.Lock()
	l.result = l.outcomeLocked(waitErr)
	l.mu.Unlock()
	close(l.done)
}

// signInURL accepts only Google's OAuth endpoint, so nothing else agy
// prints is ever offered as a link.
func signInURL(line string) bool {
	u, err := url.Parse(line)
	return err == nil && u.Scheme == "https" && u.Host == "accounts.google.com" && strings.HasPrefix(u.Path, "/o/oauth2/")
}

func (l *Login) outcomeLocked(waitErr error) error {
	for _, line := range l.lines {
		if email, ok := strings.CutPrefix(line, loginPrefix); ok {
			l.email = strings.TrimSuffix(email, ".")
		}
	}
	if waitErr == nil && l.email != "" {
		return nil
	}
	for _, line := range l.lines {
		if strings.HasPrefix(line, loginFailPrefix) {
			return errors.New(line)
		}
	}
	if waitErr != nil {
		return fmt.Errorf("agy login: %w", waitErr)
	}
	return errors.New("agy login ended without signing in")
}

// URL is the Google sign-in link to open in a browser.
func (l *Login) URL() string { return l.url }

// Submit sends the authorization code (or the landing page URL) and waits
// for the outcome. It returns the signed-in email.
func (l *Login) Submit(ctx context.Context, code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > maxLoginCodeBytes || strings.ContainsAny(code, "\r\n") {
		return "", errors.New("paste the code or the landing page URL as one line")
	}
	if _, err := io.WriteString(l.stdin, code+"\n"); err != nil {
		return "", fmt.Errorf("agy login: %w", err)
	}
	timer := time.NewTimer(loginSubmitTimeout)
	defer timer.Stop()
	select {
	case <-l.done:
	case <-timer.C:
		l.Close()
		return "", errors.New("agy login did not finish")
	case <-ctx.Done():
		l.Close()
		return "", ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.email, l.result
}

// Err is the outcome once the login ended.
func (l *Login) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.result
}

// Close kills the login if it still runs.
func (l *Login) Close() {
	select {
	case <-l.done:
		return
	default:
	}
	_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
	<-l.done
}
