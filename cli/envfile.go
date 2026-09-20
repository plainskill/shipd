package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultEnvFile is the env file the CLI reads when --env-path is not given.
const defaultEnvFile = ".shipd.env"

// parseEnvFile reads a dotenv-style file:
//
//	KEY=VALUE          one per line
//	# comment          blank lines and comments are ignored
//	export KEY=VALUE   an "export " prefix is accepted
//	KEY="a b"          quotes are stripped; \n \" \\ unescaping inside ""
//	KEY='a b'          single quotes are literal
//	KEY=               an empty value is an empty value (use --env-unset to remove)
//
// It never fails on a malformed line: bad lines are reported as warnings and
// skipped, so one typo cannot block a deploy.
func parseEnvFile(path string) (map[string]string, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	env := map[string]string{}
	var warns []string
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			warns = append(warns, fmt.Sprintf("%s:%d: no '=' — skipped", filepath.Base(path), i+1))
			continue
		}
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			warns = append(warns, fmt.Sprintf("%s:%d: %q is not a valid variable name — skipped", filepath.Base(path), i+1, key))
			continue
		}
		env[key] = unquoteValue(strings.TrimSpace(value))
	}
	return env, warns, nil
}

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquoteValue strips surrounding quotes (unescaping inside double quotes) and
// drops a trailing " # comment" on unquoted values.
func unquoteValue(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '"' && v[len(v)-1] == '"':
			return unescapeDouble(v[1 : len(v)-1])
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return v[1 : len(v)-1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func unescapeDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// repoRoot returns the git top level of dir, or "" when dir is not in a repo.
func repoRoot(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// trackedByGit reports whether path is tracked by git — the case where a
// secrets file is about to be committed and shipped to the server.
func trackedByGit(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	dir := filepath.Dir(abs)
	root := repoRoot(dir)
	if root == "" {
		return false
	}
	cmd := exec.Command("git", "-C", root, "ls-files", "--error-unmatch", "--", abs)
	return cmd.Run() == nil
}

// loadEnvFile resolves and reads the env file for a deploy.
//
// explicitPath (--env-path) overrides the default .shipd.env; a missing
// explicit file warns (you asked for it, so silence would be a lie), while a
// missing default file is silent (most repos have none). Returns the parsed
// values, the human label of the source, and any warnings to print.
func loadEnvFile(explicitPath, dir string) (env map[string]string, label string, warns []string) {
	if explicitPath == "" {
		// default: .shipd.env at the repo root when we are in a repo, else cwd
		base := dir
		if root := repoRoot(dir); root != "" {
			base = root
		}
		p := filepath.Join(base, defaultEnvFile)
		if _, err := os.Stat(p); err != nil {
			return nil, "", nil // silent: no .shipd.env is the normal case
		}
		vals, ws, err := parseEnvFile(p)
		if err != nil {
			return nil, "", []string{fmt.Sprintf("warn: cannot read %s: %v", p, err)}
		}
		return vals, p, append(trackWarning(p), ws...)
	}
	if _, err := os.Stat(explicitPath); err != nil {
		return nil, "", []string{fmt.Sprintf("warn: env file not found: %s", explicitPath)}
	}
	vals, ws, err := parseEnvFile(explicitPath)
	if err != nil {
		return nil, "", []string{fmt.Sprintf("warn: cannot read %s: %v", explicitPath, err)}
	}
	return vals, explicitPath, append(trackWarning(explicitPath), ws...)
}

// trackWarning is the secrets-in-git warning: a tracked env file is cloned by
// the server on every deploy, so its values end up in the repository history.
func trackWarning(path string) []string {
	if !trackedByGit(path) {
		return nil
	}
	return []string{fmt.Sprintf("warn: %s is tracked by git — secrets will ship in the deploy", filepath.Base(path))}
}
