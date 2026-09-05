package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

type config struct {
	Repo, DataPath, Attic, AtticCache string
	Hosts                             []string
	Interval                          time.Duration
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var cachePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var storePattern = regexp.MustCompile(`^/nix/store/([0-9abcdfghijklmnpqrsvwxyz]{32})-[^/\s]+$`)

func readConfig() (config, error) {
	c := config{Repo: os.Getenv("GITHUB_REPO"), DataPath: os.Getenv("DATA_PATH"), Attic: os.Getenv("ATTIC_SERVER"), AtticCache: os.Getenv("ATTIC_CACHE")}
	if !repoPattern.MatchString(c.Repo) {
		return c, errors.New("GITHUB_REPO must be owner/repository")
	}
	if c.DataPath == "" {
		c.DataPath = "/var/lib/deployer"
	}
	if !filepath.IsAbs(c.DataPath) {
		return c, errors.New("DATA_PATH must be absolute")
	}
	c.DataPath = filepath.Clean(c.DataPath)
	if c.DataPath == "/" {
		return c, errors.New("DATA_PATH must not be /")
	}
	seen := map[string]bool{}
	for h := range strings.SplitSeq(os.Getenv("HOSTS"), ",") {
		h = strings.TrimSpace(h)
		if !hostPattern.MatchString(h) {
			return c, errors.New("HOSTS must contain comma-separated host names (letters, digits, underscores or hyphens)")
		}
		if !seen[h] {
			c.Hosts = append(c.Hosts, h)
			seen[h] = true
		}
	}
	if c.Attic == "" {
		return c, errors.New("ATTIC_SERVER is required")
	}
	if !strings.Contains(c.Attic, "://") {
		c.Attic = "https://" + c.Attic
	}
	u, err := url.Parse(c.Attic)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("ATTIC_SERVER must be a domain or HTTP(S) server URL without a path")
	}
	c.Attic = strings.TrimRight(u.String(), "/")
	if !cachePattern.MatchString(c.AtticCache) {
		return c, errors.New("ATTIC_CACHE is required and must be a cache name (letters, digits, underscores or hyphens)")
	}
	interval := os.Getenv("INTERVAL")
	if interval == "" {
		interval = "60s"
	}
	c.Interval, err = time.ParseDuration(interval)
	if err != nil || c.Interval <= 0 {
		return c, errors.New("INTERVAL must be a positive Go duration")
	}
	return c, nil
}

type state struct {
	Repo     string                       `json:"repo"`
	Paths    map[string]map[string]string `json:"paths"`    // commit -> host -> system toplevel
	Ready    map[string]bool              `json:"ready"`    // cache URL + store path
	Deployed map[string]map[string]bool   `json:"deployed"` // commit -> host
}

func newState(repo string) state {
	return state{
		Repo:     repo,
		Paths:    make(map[string]map[string]string),
		Ready:    make(map[string]bool),
		Deployed: make(map[string]map[string]bool),
	}
}

type service struct {
	config config
	state  state
	run    func(context.Context, string, ...string) (string, error)
	client *http.Client
}

// Keep diagnostics bounded while suppressing subprocess progress on success.
type tailBuffer struct{ buffer bytes.Buffer }

func (b *tailBuffer) Write(p []byte) (int, error) {
	const limit = 32 * 1024
	n := len(p)
	if n >= limit {
		b.buffer.Reset()
		p = p[n-limit:]
	} else if b.buffer.Len()+n > limit {
		b.buffer.Next(b.buffer.Len() + n - limit)
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *tailBuffer) String() string { return b.buffer.String() }

type commandError struct {
	name   string
	err    error
	output string
}

func (e *commandError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("%s: %v\n%s", e.name, e.err, e.output))
}
func (e *commandError) Unwrap() error { return e.err }

// Only definite SSH transport failures are quiet. Unknown errors, DNS errors,
// authentication failures and host-key failures remain visible.
var offlineSSH = regexp.MustCompile(`(?m)ssh: connect to host [^\r\n]+ port [0-9]+: (Connection timed out|Operation timed out|Connection refused|No route to host|Network is unreachable|Host is down)(?:\r?$|\x1b)`)

func offline(err error) bool {
	var failure *commandError
	return errors.As(err, &failure) && failure.name == "deploy" && offlineSSH.MatchString(failure.output)
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Kill subprocesses too, so a stuck SSH/Nix process cannot block shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if len(output) > 32*1024 {
			output = output[len(output)-32*1024:]
		}
		return "", &commandError{name: name, err: err, output: output}
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (s *service) save() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.config.DataPath, "state.json"), data, 0600)
}

func (s *service) checkout(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	path := filepath.Join(s.config.DataPath, "flake")
	remote := "https://github.com/" + s.config.Repo + ".git"
	origin, err := s.run(ctx, "git", "-C", path, "remote", "get-url", "origin")
	if err != nil || origin != remote || s.state.Repo != s.config.Repo {
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
		if _, err := s.run(ctx, "git", "clone", "--quiet", "--depth=1", "--", remote, path); err != nil {
			return "", err
		}
		s.state = newState(s.config.Repo)
		if err := s.save(); err != nil {
			return "", err
		}
	}
	// Fetch the remote's HEAD each time, including after a default-branch rename.
	if _, err := s.run(ctx, "git", "-C", path, "fetch", "--quiet", "--depth=1", "--force", "origin", "HEAD"); err != nil {
		return "", err
	}
	commit, err := s.run(ctx, "git", "-C", path, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	if _, err := s.run(ctx, "git", "-C", path, "checkout", "--quiet", "--detach", "--force", commit); err != nil {
		return "", err
	}
	return commit, nil
}

func (s *service) source(commit string) string {
	u := url.URL{Scheme: "git+file", Path: filepath.Join(s.config.DataPath, "flake")}
	q := url.Values{"rev": {commit}, "shallow": {"1"}}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *service) cacheURL() string {
	return s.config.Attic + "/" + s.config.AtticCache
}

func (s *service) ready(ctx context.Context, path string) (bool, error) {
	match := storePattern.FindStringSubmatch(path)
	if match == nil {
		return false, fmt.Errorf("invalid store path %q", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.cacheURL()+"/"+match[1]+".narinfo", nil)
	if err != nil {
		return false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("Attic HEAD %s: %s", req.URL, resp.Status)
	}
}

func (s *service) host(ctx context.Context, commit, host string) error {
	if s.state.Deployed[commit][host] {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	path := s.state.Paths[commit][host]
	if path == "" {
		var err error
		path, err = s.run(ctx, "nix", "eval", "--raw", "--no-write-lock-file", s.source(commit)+"#nixosConfigurations."+host+".config.system.build.toplevel")
		if err != nil {
			return err
		}
		if !storePattern.MatchString(path) {
			return fmt.Errorf("invalid system toplevel %q", path)
		}
		s.state.Paths[commit][host] = path
		if err := s.save(); err != nil {
			return err
		}
	}
	key := s.cacheURL() + "|" + path
	if !s.state.Ready[key] {
		ready, err := s.ready(ctx, path)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		s.state.Ready[key] = true
		if err := s.save(); err != nil {
			return err
		}
		log.Printf("%s: configuration available in Attic (%s)", host, commit)
	}

	_, err := s.run(ctx, "deploy", s.source(commit)+"#"+host+".system",
		"--boot", "--magic-rollback", "false", "--auto-rollback", "false",
		"--rollback-succeeded", "false", "--interactive-sudo", "false", "--skip-checks",
		"--", "--no-write-lock-file")
	if err != nil {
		if offline(err) {
			return nil
		}
		return err
	}
	s.state.Deployed[commit][host] = true
	if err := s.save(); err != nil {
		return err
	}
	log.Printf("%s: deployed %s for next boot", host, commit)
	return nil
}

// Discard old revisions, removed hosts and cache entries no longer in use.
func (s *service) prune(commit string) error {
	size := func() int {
		return len(s.state.Paths) + len(s.state.Deployed) + len(s.state.Ready) +
			len(s.state.Paths[commit]) + len(s.state.Deployed[commit])
	}
	before := size()
	maps.DeleteFunc(s.state.Paths, func(revision string, _ map[string]string) bool { return revision != commit })
	maps.DeleteFunc(s.state.Deployed, func(revision string, _ map[string]bool) bool { return revision != commit })
	maps.DeleteFunc(s.state.Paths[commit], func(host, _ string) bool { return !slices.Contains(s.config.Hosts, host) })
	maps.DeleteFunc(s.state.Deployed[commit], func(host string, _ bool) bool { return !slices.Contains(s.config.Hosts, host) })
	active := make(map[string]bool)
	for _, path := range s.state.Paths[commit] {
		active[s.cacheURL()+"|"+path] = true
	}
	maps.DeleteFunc(s.state.Ready, func(key string, _ bool) bool { return !active[key] })
	if size() != before {
		return s.save()
	}
	return nil
}

func (s *service) poll(ctx context.Context) error {
	commit, err := s.checkout(ctx)
	if err != nil {
		return err
	}
	if err := s.prune(commit); err != nil {
		return err
	}
	if s.state.Paths[commit] == nil {
		s.state.Paths[commit] = map[string]string{}
	}
	if s.state.Deployed[commit] == nil {
		s.state.Deployed[commit] = map[string]bool{}
	}
	for _, host := range s.config.Hosts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.host(ctx, commit, host); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("%s: %v (will retry)", host, err)
		}
	}
	return nil
}

func run(ctx context.Context) error {
	c, err := readConfig()
	if err != nil {
		return err
	}
	for _, name := range []string{"git", "nix", "deploy"} {
		if _, err := exec.LookPath(name); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(c.DataPath, 0700); err != nil {
		return err
	}
	c.DataPath, err = filepath.EvalSymlinks(c.DataPath)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.DataPath, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock DATA_PATH: %w", err)
	}
	s := service{config: c, state: newState(""), run: command, client: &http.Client{Timeout: 30 * time.Second}}
	b, err := os.ReadFile(filepath.Join(c.DataPath, "state.json"))
	if err == nil {
		if err := json.Unmarshal(b, &s.state); err != nil {
			return fmt.Errorf("read state: %w", err)
		}
		if s.state.Paths == nil || s.state.Ready == nil || s.state.Deployed == nil {
			return errors.New("invalid state maps")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		if err := s.poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("poll: %v (will retry)", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
