package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".shipd.env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseEnvFile(t *testing.T) {
	body := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"SPACED =  padded  ",
		"export EXPORTED=yes",
		`DOUBLE="hello world"`,
		`ESCAPED="line1\nline2\"q\""`,
		`SINGLE='literal $NOT_EXPANDED'`,
		"COMMENTED=value # trailing comment",
		"EMPTY=",
		`QUOTED_HASH="a # b"`,
		"DUP=first",
		"DUP=second",
	}, "\n") + "\n"
	p := writeTempEnv(t, body)

	env, warns, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	want := map[string]string{
		"PLAIN":       "value",
		"SPACED":      "padded",
		"EXPORTED":    "yes",
		"DOUBLE":      "hello world",
		"ESCAPED":     "line1\nline2\"q\"",
		"SINGLE":      "literal $NOT_EXPANDED",
		"COMMENTED":   "value",
		"EMPTY":       "",
		"QUOTED_HASH": "a # b",
		"DUP":         "second",
	}
	for k, v := range want {
		if got, ok := env[k]; !ok {
			t.Errorf("%s missing", k)
		} else if got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(env), len(want), env)
	}
}

func TestParseEnvFileSkipsBadLinesWithoutFailing(t *testing.T) {
	p := writeTempEnv(t, "GOOD=1\nno equals here\n1BAD=x\nBAD-KEY=y\nALSO_GOOD=2\n")
	env, warns, err := parseEnvFile(p)
	if err != nil {
		t.Fatalf("a malformed line must not fail the parse: %v", err)
	}
	if len(env) != 2 || env["GOOD"] != "1" || env["ALSO_GOOD"] != "2" {
		t.Errorf("good lines lost: %v", env)
	}
	if len(warns) != 3 {
		t.Errorf("expected 3 warnings (no '=', 1BAD, BAD-KEY), got %d: %v", len(warns), warns)
	}
}

func TestParseEnvFileCRLF(t *testing.T) {
	p := writeTempEnv(t, "A=1\r\nB=2\r\n")
	env, _, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "2" || len(env) != 2 {
		t.Errorf("CRLF handling: %v", env)
	}
}

func TestValidEnvKey(t *testing.T) {
	ok := []string{"A", "_A", "A_B", "a1", "__x__"}
	bad := []string{"", "1A", "A-B", "A B", "A.B", "A="}
	for _, k := range ok {
		if !validEnvKey(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range bad {
		if validEnvKey(k) {
			t.Errorf("%q should be invalid", k)
		}
	}
}

// the default file is silent when absent: most repos have no .shipd.env
func TestLoadEnvFileSilentWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	env, label, warns := loadEnvFile("", dir)
	if env != nil || label != "" || len(warns) != 0 {
		t.Errorf("missing default file should be silent, got env=%v label=%q warns=%v", env, label, warns)
	}
}

// an explicitly requested file that is missing must warn
func TestLoadEnvFileWarnsWhenExplicitMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, warns := loadEnvFile(filepath.Join(dir, "nope.env"), dir)
	if len(warns) != 1 || !strings.Contains(warns[0], "env file not found") {
		t.Errorf("expected a not-found warning, got %v", warns)
	}
}

// --env-path overrides .shipd.env entirely
func TestLoadEnvFileExplicitOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".shipd.env"), []byte("FROM=DEFAULT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(custom, []byte("FROM=CUSTOM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, warns := loadEnvFile(custom, dir)
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if env["FROM"] != "CUSTOM" {
		t.Errorf("explicit env-path did not override the default file: %v", env)
	}
}
