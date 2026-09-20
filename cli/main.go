// Command shipd is the client CLI for a shipd deploy plane.
//
// shipd is host-agnostic: the server URL is chosen at login time and stored
// with the token, so nothing here is hardcoded to a particular deployment.
//
//	shipd login [token]                           connect to a shipd server
//	shipd deploy [subdomain]                      deploy the repo you're in
//	shipd delete [subdomain]                      remove a deployment
//	shipd apps                                    list deployments
//	shipd logs [subdomain]                        container logs
//	shipd whoami / logout / version
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev"

// app mirrors the server's App record (only the fields the CLI shows).
type app struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	Subdomain  string `json:"subdomain"`
	Domain     string `json:"domain"`
	Status     string `json:"status"`
	GitSHA     string `json:"git_sha"`
	LastError  string `json:"last_error"`
	DesiredUp  bool   `json:"desired_up"`
	LastDeploy string `json:"last_deploy"`
}

// ---- client config (~/.config/shipd/config.json, 0600) ---------------

type cliConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "shipd", "config.json")
}

func loadConfig() cliConfig {
	var c cliConfig
	p := configPath()
	if p == "" {
		return c
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c cliConfig) error {
	p := configPath()
	if p == "" {
		return errors.New("cannot determine home directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// server resolves the target: SHIPD_SERVER env wins, then stored config.
// No default — shipd is host-agnostic.
func server() (string, error) {
	if s := strings.TrimSpace(os.Getenv("SHIPD_SERVER")); s != "" {
		return strings.TrimRight(s, "/"), nil
	}
	if s := loadConfig().Server; s != "" {
		return strings.TrimRight(s, "/"), nil
	}
	return "", errors.New("no shipd server configured — run `shipd login`")
}

func token() (string, error) {
	if t := strings.TrimSpace(os.Getenv("SHIPD_TOKEN")); t != "" {
		return t, nil
	}
	if t := loadConfig().Token; t != "" {
		return t, nil
	}
	return "", errors.New("no API token — run `shipd login` (create one in the dashboard)")
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "deploy":
		cmdDeploy(args[1:])
	case "delete", "undeploy", "rm":
		cmdDelete(args[1:])
	case "apps", "ls", "list":
		cmdApps()
	case "logs":
		cmdLogs(args[1:])
	case "login":
		cmdLogin(args[1:])
	case "logout":
		cmdLogout()
	case "whoami":
		cmdWhoami()
	case "version", "-v", "--version":
		fmt.Println("shipd client", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "shipd: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`shipd — deploy git repos to a shipd server

usage:
  shipd login [token]
        connect to a shipd server: asks for the url (defaults to the stored
        one) and the token, verifies it, and stores both in
        ~/.config/shipd/config.json

  shipd deploy [subdomain] [--branch <b>] [--repo <url>]
        deploy the repo in the current directory. with no subdomain:
        redeploys in place if this repo+branch already exists, otherwise
        picks a random subdomain (printed in the response).

  shipd delete [subdomain] [--branch <b>] [--repo <url>]
        remove this repo's deployment (or the named one).

  shipd apps                    list deployments
  shipd logs [subdomain]        container logs (last 200 lines)
  shipd whoami                  show the server and token in use
  shipd logout                  forget the stored token
  shipd version

environment (skip the prompts / override stored config):
  SHIPD_SERVER    server base url
  SHIPD_TOKEN     API token
`)
}

// ---- terminal helpers -------------------------------------------------

// promptToken reads a secret from the terminal with echo disabled, so it
// never lands in shell history or on screen.
func promptToken(label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	echoOff := off.Run() == nil
	var line string
	_, err := fmt.Fscanln(os.Stdin, &line)
	if echoOff {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
		fmt.Fprintln(os.Stderr)
	}
	if err != nil {
		die("could not read input: %v", err)
	}
	return strings.TrimSpace(line)
}

func promptLine(label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// ---- git discovery ----------------------------------------------------

func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// repoURL reads origin and normalizes it to a form shipd accepts. ssh/scp
// remotes are rewritten to https (shipd clones server-side; pass --repo with
// a git@ url for key-based access to private repos).
func repoURL() (string, error) {
	raw, err := gitOut("remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("not a git repo with an 'origin' remote (use --repo)")
	}
	return normalizeRemote(raw), nil
}

func normalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "http://"):
		return raw
	case strings.HasPrefix(raw, "ssh://"):
		rest := strings.TrimPrefix(raw, "ssh://")
		rest = strings.TrimPrefix(rest, "git@")
		host, path, ok := strings.Cut(rest, "/")
		if !ok {
			return raw
		}
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
		return "https://" + host + "/" + path
	case strings.HasPrefix(raw, "git@"):
		rest := strings.TrimPrefix(raw, "git@")
		host, path, ok := strings.Cut(rest, ":")
		if !ok {
			return raw
		}
		return "https://" + host + "/" + path
	}
	return raw
}

func currentBranch() (string, error) {
	b, err := gitOut("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if b == "HEAD" {
		return "", errors.New("detached HEAD — pass --branch <name>")
	}
	return b, nil
}

// ---- api --------------------------------------------------------------

func api(method, path string, body any) (int, []byte, error) {
	srv, err := server()
	if err != nil {
		return 0, nil, err
	}
	tok, err := token()
	if err != nil {
		return 0, nil, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Basic "+basicAuth("shipd", tok))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

func basicAuth(user, pass string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(user + ":" + pass)
	var b strings.Builder
	for i := 0; i < len(src); i += 3 {
		var n uint32
		rem := len(src) - i
		n = uint32(src[i]) << 16
		if rem > 1 {
			n |= uint32(src[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(src[i+2])
		}
		b.WriteByte(table[(n>>18)&63])
		b.WriteByte(table[(n>>12)&63])
		if rem > 1 {
			b.WriteByte(table[(n>>6)&63])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(table[n&63])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "shipd: "+format+"\n", a...)
	os.Exit(1)
}

// ---- commands ---------------------------------------------------------

func cmdLogin(args []string) {
	var tok string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			die("unknown flag %q (login takes an optional token only)", a)
		}
		tok = a
	}

	// the server is chosen interactively: env override, then prompt
	srv := strings.TrimSpace(os.Getenv("SHIPD_SERVER"))
	if srv == "" {
		srv = promptLine("shipd server url", loadConfig().Server)
	}
	if srv == "" {
		die("no server url given")
	}
	srv = strings.TrimRight(srv, "/")
	if !strings.HasPrefix(srv, "http://") && !strings.HasPrefix(srv, "https://") {
		srv = "https://" + srv
	}
	if tok == "" {
		tok = promptToken("API token")
	}
	if tok == "" {
		die("no token given")
	}

	req, err := http.NewRequest("GET", srv+"/api/whoami", nil)
	if err != nil {
		die("%v", err)
	}
	req.Header.Set("Authorization", "Basic "+basicAuth("shipd", tok))
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		die("cannot reach %s: %v", srv, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		die("token rejected by %s (%d)", srv, resp.StatusCode)
	}
	var who struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	_ = json.Unmarshal(body, &who)

	if err := saveConfig(cliConfig{Server: srv, Token: tok}); err != nil {
		die("%v", err)
	}
	name := who.Name
	if name == "" {
		name = "unknown"
	}
	fmt.Printf("logged in as %s → %s\ntoken stored in %s\n", name, srv, configPath())
}

func cmdLogout() {
	c := loadConfig()
	if err := saveConfig(cliConfig{Server: c.Server}); err != nil {
		die("%v", err)
	}
	fmt.Println("stored token removed")
}

func cmdWhoami() {
	srv, err := server()
	if err != nil {
		die("%v", err)
	}
	code, body, err := api("GET", "/api/whoami", nil)
	if err != nil {
		die("%v", err)
	}
	if code != http.StatusOK {
		die("not authenticated against %s (%d)", srv, code)
	}
	var who struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(body, &who); err != nil {
		die("bad response: %v", err)
	}
	src := "config"
	if strings.TrimSpace(os.Getenv("SHIPD_TOKEN")) != "" {
		src = "SHIPD_TOKEN"
	}
	fmt.Printf("%s (id %s)\nserver: %s\ntoken source: %s\n", who.Name, who.ID, srv, src)
}

func cmdDeploy(args []string) {
	var subdomain, branch, repo string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--branch", "-b":
			if i+1 >= len(args) {
				die("--branch needs a value")
			}
			i++
			branch = args[i]
		case "--repo", "-r":
			if i+1 >= len(args) {
				die("--repo needs a value")
			}
			i++
			repo = normalizeRemote(args[i])
		default:
			if strings.HasPrefix(args[i], "-") {
				die("unknown flag %q", args[i])
			}
			if subdomain != "" {
				die("only one subdomain may be given")
			}
			subdomain = args[i]
		}
	}
	if repo == "" {
		var err error
		repo, err = repoURL()
		if err != nil {
			die("%v", err)
		}
	}
	if branch == "" {
		var err error
		branch, err = currentBranch()
		if err != nil {
			die("%v", err)
		}
	}

	code, body, err := api("POST", "/api/deploy", map[string]string{
		"repo": repo, "branch": branch, "subdomain": subdomain,
	})
	if err != nil {
		die("%v", err)
	}
	if code != http.StatusAccepted {
		die("deploy rejected (%d): %s", code, apiErr(body))
	}
	var resp struct {
		App app `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		die("bad response: %v", err)
	}
	if subdomain == "" {
		fmt.Printf("no subdomain given — using %s\n", resp.App.Subdomain)
	}
	fmt.Printf("deploying %s@%s → https://%s\n", repo, branch, resp.App.Domain)

	deadline := time.Now().Add(10 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		a, err := findApp(resp.App.Subdomain)
		if err == nil && a != nil {
			if a.Status != last {
				fmt.Printf("  %s\n", a.Status)
				last = a.Status
			}
			switch a.Status {
			case "running":
				fmt.Printf("live at https://%s\n", a.Domain)
				return
			case "failed":
				die("deploy failed: %s", a.LastError)
			}
		}
		time.Sleep(2 * time.Second)
	}
	die("timed out waiting for %s (check `shipd apps`)", resp.App.Subdomain)
}

func findApp(sub string) (*app, error) {
	code, body, err := api("GET", "/api/apps", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list failed (%d): %s", code, apiErr(body))
	}
	var apps []app
	if err := json.Unmarshal(body, &apps); err != nil {
		return nil, err
	}
	for i := range apps {
		if apps[i].Subdomain == sub {
			return &apps[i], nil
		}
	}
	return nil, nil
}

func cmdDelete(args []string) {
	var subdomain, branch, repo string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--branch", "-b":
			if i+1 >= len(args) {
				die("--branch needs a value")
			}
			i++
			branch = args[i]
		case "--repo", "-r":
			if i+1 >= len(args) {
				die("--repo needs a value")
			}
			i++
			repo = normalizeRemote(args[i])
		default:
			subdomain = args[i]
		}
	}

	if subdomain != "" {
		code, body, err := api("POST", "/api/apps/"+subdomain+"/delete", nil)
		if err != nil {
			die("%v", err)
		}
		if code != http.StatusAccepted {
			die("delete rejected (%d): %s", code, apiErr(body))
		}
		fmt.Printf("deleted %s\n", subdomain)
		return
	}

	if repo == "" {
		var err error
		repo, err = repoURL()
		if err != nil {
			die("%v", err)
		}
	}
	if branch == "" {
		var err error
		branch, err = currentBranch()
		if err != nil {
			die("%v", err)
		}
	}
	code, body, err := api("POST", "/api/delete", map[string]string{"repo": repo, "branch": branch})
	if err != nil {
		die("%v", err)
	}
	if code == http.StatusNotFound {
		die("nothing deployed for %s@%s", repo, branch)
	}
	if code != http.StatusAccepted {
		die("delete rejected (%d): %s", code, apiErr(body))
	}
	var resp struct {
		Deleted string `json:"deleted"`
	}
	_ = json.Unmarshal(body, &resp)
	fmt.Printf("deleted %s (%s@%s)\n", resp.Deleted, repo, branch)
}

func cmdApps() {
	code, body, err := api("GET", "/api/apps", nil)
	if err != nil {
		die("%v", err)
	}
	if code != http.StatusOK {
		die("list failed (%d): %s", code, apiErr(body))
	}
	var apps []app
	if err := json.Unmarshal(body, &apps); err != nil {
		die("bad response: %v", err)
	}
	if len(apps) == 0 {
		fmt.Println("no deployments")
		return
	}
	for _, a := range apps {
		fmt.Printf("%-24s %-10s https://%s\n", a.Subdomain, a.Status, a.Domain)
		fmt.Printf("%-24s %s@%s %s\n", "", shortRepo(a.Repo), a.Branch, a.GitSHA)
		if a.LastError != "" {
			fmt.Printf("%-24s error: %s\n", "", a.LastError)
		}
	}
}

func shortRepo(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git@")
	return strings.TrimSuffix(u, ".git")
}

func cmdLogs(args []string) {
	subdomain := ""
	if len(args) > 0 {
		subdomain = args[0]
	} else {
		repo, err := repoURL()
		if err != nil {
			die("%v", err)
		}
		branch, err := currentBranch()
		if err != nil {
			die("%v", err)
		}
		code, body, err := api("GET", "/api/apps", nil)
		if err != nil {
			die("%v", err)
		}
		if code != http.StatusOK {
			die("list failed (%d): %s", code, apiErr(body))
		}
		var apps []app
		_ = json.Unmarshal(body, &apps)
		for _, a := range apps {
			if a.Repo == repo && a.Branch == branch {
				subdomain = a.Subdomain
				break
			}
		}
		if subdomain == "" {
			die("no deployment for %s@%s", repo, branch)
		}
	}
	code, body, err := api("GET", "/api/apps/"+subdomain+"/logs?lines=200", nil)
	if err != nil {
		die("%v", err)
	}
	if code != http.StatusOK {
		die("logs failed (%d): %s", code, apiErr(body))
	}
	fmt.Print(string(body))
}

func apiErr(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
}
