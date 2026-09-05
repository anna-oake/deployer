package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testPath = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixos-system-eule"

func TestConfig(t *testing.T) {
	for k, v := range map[string]string{"GITHUB_REPO": "anna-oake/nixos-config", "HOSTS": " eule,star,eule ", "ATTIC_SERVER": "attic.oa.ke", "ATTIC_CACHE": "nixos", "DATA_PATH": "", "INTERVAL": ""} {
		t.Setenv(k, v)
	}
	c, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.String() != "1m0s" || c.DataPath != "/var/lib/deployer" || c.Attic != "https://attic.oa.ke" || c.AtticCache != "nixos" || !reflect.DeepEqual(c.Hosts, []string{"eule", "star"}) {
		t.Fatalf("unexpected config: %+v", c)
	}
	for _, tc := range []struct{ k, v string }{{"INTERVAL", "0s"}, {"INTERVAL", "oops"}, {"HOSTS", ""}, {"HOSTS", "eule,"}, {"GITHUB_REPO", "../bad"}, {"ATTIC_SERVER", "https://"}, {"ATTIC_SERVER", "attic.oa.ke/cache"}, {"ATTIC_CACHE", ""}, {"ATTIC_CACHE", "../cache"}, {"DATA_PATH", "/"}} {
		t.Run(tc.k+tc.v, func(t *testing.T) {
			t.Setenv(tc.k, tc.v)
			if _, err := readConfig(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestRetryAndPersistence(t *testing.T) {
	status, heads, evals, deploys := 404, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heads++
		if r.Method != http.MethodHead || r.URL.Path != "/cache/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.narinfo" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	s := service{config: config{DataPath: t.TempDir(), Attic: server.URL, AtticCache: "cache"}, state: newState("repo"), client: server.Client()}
	s.state.Paths["commit"] = map[string]string{}
	s.state.Deployed["commit"] = map[string]bool{}
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "nix" {
			evals++
			return testPath, nil
		}
		deploys++
		want := []string{s.source("commit") + "#eule.system", "--boot", "--magic-rollback", "false", "--auto-rollback", "false", "--rollback-succeeded", "false", "--interactive-sudo", "false", "--skip-checks", "--", "--no-write-lock-file"}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("deploy args: %q", args)
		}
		if deploys == 1 {
			return "", errors.New("host asleep")
		}
		return "", nil
	}
	attempt := func() error { return s.host(context.Background(), "commit", "eule") }
	if err := attempt(); err != nil {
		t.Fatal(err)
	}
	if err := attempt(); err != nil {
		t.Fatal(err)
	}
	if evals != 1 || heads != 2 || deploys != 0 {
		t.Fatal("missing paths must be retried without reevaluation or deployment")
	}
	status = 200
	if err := attempt(); err == nil {
		t.Fatal("expected deployment failure")
	}
	// Restart from disk: cached readiness and evaluation must survive a failed deploy.
	b, err := os.ReadFile(filepath.Join(s.config.DataPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.state = newState("")
	if err := json.Unmarshal(b, &s.state); err != nil {
		t.Fatal(err)
	}
	if err := attempt(); err != nil {
		t.Fatal(err)
	}
	// Restart after success: the deployment marker must also survive on disk.
	b, err = os.ReadFile(filepath.Join(s.config.DataPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.state = newState("")
	if err := json.Unmarshal(b, &s.state); err != nil {
		t.Fatal(err)
	}
	if err := attempt(); err != nil {
		t.Fatal(err)
	}
	if evals != 1 || heads != 3 || deploys != 2 || !s.state.Deployed["commit"]["eule"] {
		t.Fatalf("evals=%d heads=%d deploys=%d", evals, heads, deploys)
	}
	// Cache facts are scoped to the cache URL.
	s.config.AtticCache = "other"
	s.state.Deployed["commit"]["eule"] = false
	if s.state.Ready[s.cacheURL()+"|"+testPath] {
		t.Fatal("cache hit leaked across servers")
	}
}

func TestHeadErrorsNeverFallBack(t *testing.T) {
	for _, status := range []int{401, 405, 500} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != "HEAD" {
				t.Error("GET fallback")
			}
			w.WriteHeader(status)
		}))
		s := service{config: config{Attic: server.URL, AtticCache: "cache"}, client: server.Client()}
		ready, err := s.ready(context.Background(), testPath)
		server.Close()
		if ready || err == nil || calls != 1 {
			t.Fatalf("status %d: ready=%v err=%v calls=%d", status, ready, err, calls)
		}
	}
}

func TestRepositoryChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "flake"), 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "flake", "stale")
	if err := os.WriteFile(stale, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	s := service{config: config{Repo: "new/repo", DataPath: dir}, state: newState("old/repo")}
	s.state.Ready["old"] = true
	var calls []string
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.Contains(call, "get-url"):
			return "https://github.com/old/repo.git", nil
		case strings.HasPrefix(call, "clone"):
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Fatal("old checkout not wiped")
			}
		case strings.Contains(call, "rev-parse"):
			return "abc123", nil
		}
		return "", nil
	}
	commit, err := s.checkout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if commit != "abc123" || s.state.Repo != "new/repo" || len(s.state.Ready) != 0 {
		t.Fatal("repository state not reset")
	}
	if !strings.Contains(strings.Join(calls, "\n"), "fetch --quiet --depth=1 --force origin HEAD") {
		t.Fatal("did not fetch default branch")
	}
}

// Optional integration check: evaluates the pinned source with real Git/Nix.
func TestNixSource(t *testing.T) {
	if os.Getenv("DEPLOYER_NIX_TEST") != "1" {
		t.Skip("set DEPLOYER_NIX_TEST=1 to run Git/Nix integration")
	}
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "flake")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := `{ outputs = { self }: {
 nixosConfigurations.eule.config.system.build.toplevel = "` + testPath + `";
 deploy.nodes.eule = {
 hostname = "eule";
 remoteBuild = true;
 profiles.system = { remoteBuild = true; path = "` + testPath + `"; };
 };
 }; }`
	if err := os.WriteFile(filepath.Join(repo, "flake.nix"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "add", "flake.nix"}, {"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"}} {
		if _, err := command(ctx, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := command(ctx, "git", "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	s := service{config: config{DataPath: dir}}
	got, err := command(ctx, "nix", "eval", "--json", s.source(commit)+"#deploy.nodes.eule.profiles.system.remoteBuild")
	if err != nil {
		t.Fatal(err)
	}
	if got != "true" {
		t.Fatalf("remote build setting changed: %s", got)
	}
	got, err = command(ctx, "nix", "eval", "--raw", s.source(commit)+"#nixosConfigurations.eule.config.system.build.toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if got != testPath {
		t.Fatalf("path = %s", got)
	}
}

func TestPollIndependentHostsAndNewCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	s := service{config: config{Repo: "owner/repo", DataPath: t.TempDir(), Attic: server.URL, AtticCache: "cache", Hosts: []string{"eule", "star"}}, state: newState("owner/repo"), client: server.Client()}
	commit := "first"
	evals := 0
	deploys := map[string]int{}
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "git":
			call := strings.Join(args, " ")
			if strings.Contains(call, "get-url") {
				return "https://github.com/owner/repo.git", nil
			}
			if strings.Contains(call, "rev-parse") {
				return commit, nil
			}
			if strings.Contains(call, "clone") {
				t.Fatal("recloned unchanged repository")
			}
		case "nix":
			evals++
			return testPath, nil
		case "deploy":
			host := "star"
			if strings.HasSuffix(args[0], "#eule.system") {
				host = "eule"
			}
			deploys[host]++
			if host == "eule" {
				return "", errors.New("offline")
			}
		}
		return "", nil
	}
	for range 2 {
		if err := s.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if evals != 2 || deploys["eule"] != 2 || deploys["star"] != 1 {
		t.Fatalf("unexpected retries: evals=%d deploys=%v", evals, deploys)
	}
	commit = "second"
	if err := s.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evals != 4 || deploys["star"] != 2 {
		t.Fatal("new commit not processed independently")
	}
	commit = "first"
	if err := s.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evals != 6 || deploys["star"] != 3 {
		t.Fatal("returning to an old commit must deploy again after its history was discarded")
	}
}

func TestQuietPolling(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	status := 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer server.Close()
	s := service{config: config{Repo: "owner/repo", DataPath: t.TempDir(), Attic: server.URL, AtticCache: "cache", Hosts: []string{"eule"}}, state: newState("owner/repo"), client: server.Client()}
	deploys := 0
	var deployErr error = &commandError{name: "deploy", err: errors.New("exit status 1"), output: "ssh: connect to host eule port 22: Connection timed out"}
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "git":
			call := strings.Join(args, " ")
			if strings.Contains(call, "get-url") {
				return "https://github.com/owner/repo.git", nil
			}
			if strings.Contains(call, "rev-parse") {
				return "commit", nil
			}
			if !strings.Contains(call, "--quiet") {
				t.Errorf("no quiet flag: %s", call)
			}
		case "nix":
			return testPath, nil
		case "deploy":
			deploys++
			return "", deployErr
		}
		return "", nil
	}
	poll := func() {
		t.Helper()
		if err := s.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	poll()
	if logs.Len() != 0 || deploys != 0 {
		t.Fatalf("waiting for cache should be silent: %s", &logs)
	}
	status = 200
	poll()
	poll()
	if strings.Count(logs.String(), "configuration available in Attic") != 1 || strings.Contains(logs.String(), "will retry") {
		t.Fatalf("unexpected offline logs: %s", &logs)
	}
	if deploys != 2 {
		t.Fatal("offline deploys must retry on each poll")
	}
	// Readiness is persistent, so restarting while offline must remain quiet.
	b, err := os.ReadFile(filepath.Join(s.config.DataPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.state = newState("")
	if err := json.Unmarshal(b, &s.state); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	poll()
	if logs.Len() != 0 {
		t.Fatalf("restart repeated cache log: %s", &logs)
	}
	deployErr = &commandError{name: "deploy", err: errors.New("exit status 1"), output: "activation failed: permission denied"}
	poll()
	if !strings.Contains(logs.String(), "activation failed: permission denied") {
		t.Fatalf("lost real failure: %s", &logs)
	}
	logs.Reset()
	deployErr = nil
	poll()
	poll()
	if strings.Count(logs.String(), "deployed commit for next boot") != 1 || deploys != 5 {
		t.Fatalf("unexpected success logs/count: %s / %d", &logs, deploys)
	}
}

func TestOfflineClassification(t *testing.T) {
	for _, reason := range []string{"Connection timed out", "Operation timed out", "Connection refused", "No route to host", "Network is unreachable", "Host is down"} {
		err := &commandError{name: "deploy", err: errors.New("exit status 1"), output: "ssh: connect to host eule port 2222: " + reason + "\r\n"}
		if !offline(err) {
			t.Errorf("not recognized: %s", reason)
		}
	}
	for _, output := range []string{"Permission denied (publickey).", "Host key verification failed.", "ssh: Could not resolve hostname eule: Name or service not known", "activation failed", "build failed", "exit status 255"} {
		if offline(&commandError{name: "deploy", err: errors.New("failed"), output: output}) {
			t.Errorf("hidden real error: %s", output)
		}
	}
}

func TestCommandDiagnostics(t *testing.T) {
	out, err := command(context.Background(), "sh", "-c", "printf result; printf progress >&2")
	if err != nil || out != "result" {
		t.Fatalf("stdout=%q err=%v", out, err)
	}
	_, err = command(context.Background(), "sh", "-c", "printf detail >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "detail") {
		t.Fatalf("lost stderr: %v", err)
	}
	out, err = command(context.Background(), "sh", "-c", "printf '%040000d' 0; printf end")
	if err != nil || len(out) != 32*1024 || !strings.HasSuffix(out, "end") {
		t.Fatalf("command output was not bounded: length=%d err=%v", len(out), err)
	}
	var tail tailBuffer
	for range 100 {
		_, _ = tail.Write(bytes.Repeat([]byte("x"), 1024))
	}
	if len(tail.String()) != 32*1024 {
		t.Fatalf("unbounded diagnostics: %d", len(tail.String()))
	}
}

func TestPruneState(t *testing.T) {
	s := service{config: config{DataPath: t.TempDir(), Attic: "https://cache.example", AtticCache: "nixos", Hosts: []string{"eule"}}, state: newState("owner/repo")}
	s.state.Paths["old"] = map[string]string{"eule": testPath}
	s.state.Deployed["old"] = map[string]bool{"eule": true}
	s.state.Paths["current"] = map[string]string{"eule": testPath, "removed": testPath + "-old"}
	s.state.Deployed["current"] = map[string]bool{"eule": true, "removed": true}
	key := s.cacheURL() + "|" + testPath
	s.state.Ready[key] = true
	s.state.Ready[key+"-old"] = true
	s.state.Ready["https://previous-cache.example|"+testPath] = true
	if err := s.prune("current"); err != nil {
		t.Fatal(err)
	}
	var persisted state
	b, err := os.ReadFile(filepath.Join(s.config.DataPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Paths) != 1 || len(persisted.Paths["current"]) != 1 ||
		len(persisted.Deployed) != 1 || len(persisted.Deployed["current"]) != 1 ||
		!persisted.Deployed["current"]["eule"] || len(persisted.Ready) != 1 || !persisted.Ready[key] {
		t.Fatalf("unexpected retained state: %+v", persisted)
	}
}

func TestAtticTokenFile(t *testing.T) {
	for key, value := range map[string]string{
		"GITHUB_REPO": "owner/repo", "HOSTS": "eule", "ATTIC_SERVER": "attic.example",
		"ATTIC_CACHE": "nixos", "DATA_PATH": t.TempDir(), "INTERVAL": "60s",
	} {
		t.Setenv(key, value)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	t.Setenv("ATTIC_TOKEN_FILE", tokenFile)
	if _, err := readConfig(); err == nil {
		t.Fatal("missing token file accepted")
	}
	for _, invalid := range []string{"", "\n", "token\nsecond-line"} {
		if err := os.WriteFile(tokenFile, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readConfig(); err == nil {
			t.Fatal("invalid token file accepted")
		}
	}
	const token = "test-attic-token"
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "HEAD" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c.Attic = server.URL
	s := service{config: c, state: newState(c.Repo), client: server.Client()}
	if ready, err := s.ready(context.Background(), testPath); err != nil || !ready {
		t.Fatalf("authenticated HEAD: ready=%v err=%v", ready, err)
	}
	s.config.AtticToken = ""
	if ready, err := s.ready(context.Background(), testPath); err == nil || ready {
		t.Fatal("unauthenticated HEAD should fail")
	}
	if calls != 2 {
		t.Fatalf("unexpected retries: %d", calls)
	}
	s.config.AtticToken = token
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(c.DataPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("token persisted in state")
	}
}
