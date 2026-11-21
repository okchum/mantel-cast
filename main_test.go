package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOwnConfig(t *testing.T) {
	t.Setenv("MANTEL_TOKEN", "from-env-0123456789")
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	c, err := loadConfig(write("mantel-cast.json", `{"mantel": "https://h.example/", "token": "$MANTEL_TOKEN",
		"cast": {"device": "Nest Hub Max"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mantel != "https://h.example/" || c.Token != "from-env-0123456789" || c.Cast.Device != "Nest Hub Max" ||
		c.Cast.AppID != "84912283" || c.Cast.Quiet != "auto" {
		t.Fatalf("mantel-cast.json: %+v", c)
	}

	if _, err := loadConfig(write("typo.json", `{"mantel": "x", "tokn": "y"}`)); err == nil {
		t.Fatal("unknown key in mantel-cast.json must fail")
	}

	// Mantel's config.json works too: only public_url, tokens, timezone, cast are read.
	c, err = loadConfig(write("config.json", `{"public_url": "http://auto:8080/", "timezone": "Pacific/Auckland",
		"tokens": [{"name": "tv", "token": "tttttttttttttttt"}, {"name": "nesthub", "token": "$MANTEL_TOKEN"}],
		"pages": [{"name": "x"}], "cast": {"quiet": "hours", "quiet_hours": "23:00-07:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mantel != "http://auto:8080/" || c.Token != "from-env-0123456789" || c.Timezone != "Pacific/Auckland" || c.Cast.Quiet != "hours" {
		t.Fatalf("config.json: %+v", c)
	}

	if _, err := loadConfig(write("bad.json", `{"cast": {"quiet": "sometimes"}}`)); err == nil {
		t.Fatal("bad cast.quiet must fail")
	}
	if c, err := loadConfig(filepath.Join(dir, "missing.json")); err != nil || c.Cast.AppID == "" {
		t.Fatalf("missing file should give defaults: %+v %v", c, err)
	}
}

func TestSharedConfig(t *testing.T) {
	parse := func(body string) (*Config, error) {
		t.Helper()
		c, err := parseConfig([]byte(body))
		if err == nil {
			err = c.Cast.Normalize()
		}
		return c, err
	}
	// Misspelt cast fields fail in Mantel's config.json too; Mantel's own keys don't.
	if _, err := parse(`{"listen": ":8080", "alerts": {"after": "2m"}, "cast": {"devcie": "Hub"}}`); err == nil || !strings.Contains(err.Error(), "devcie") {
		t.Fatalf("typo in shared cast section: %v", err)
	}
	c, err := parse(`{"listen": ":8080", "alerts": {"after": "2m"}, "cast": {"enabled": true, "device": "Hub", "pause_max": "2h"}}`)
	if err != nil || c.Cast.Device != "Hub" || !c.Cast.IsEnabled() || c.Cast.Timings().PauseMax.Hours() != 2 {
		t.Fatalf("shared: %+v %v", c, err)
	}
	// No public_url: the page Mantel serves on this machine.
	for _, c := range []struct{ body, want string }{
		{`{"listen": ":8080"}`, "http://auto:8080/"},
		{`{"tokens": []}`, "http://auto:8080/"},
		{`{"listen": "0.0.0.0:9000"}`, "http://auto:9000/"},
		{`{"listen": "[::]:9443", "tls_cert": "/etc/mantel.pem"}`, "https://auto:9443/"},
		{`{"listen": ":8080", "public_url": "https://h.example/"}`, "https://h.example/"},
	} {
		if got, err := parse(c.body); err != nil || got.Mantel != c.want {
			t.Errorf("%s: %+v, %v; want page %q", c.body, got, err, c.want)
		}
	}
	if _, err := parse(`{"listen": "8080"}`); err == nil {
		t.Error("listen without a colon should fail")
	}
	c, err = parse(`{"listen": ":8080", "cast": {"enabled": false}}`)
	if err != nil || c.Cast.IsEnabled() {
		t.Fatalf("enabled false: %+v %v", c, err)
	}
}

func TestExampleConfigParsesStrictly(t *testing.T) {
	b, err := os.ReadFile("mantel-cast.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cast.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestRunDisabledIdles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mantel-cast.json")
	if err := os.WriteFile(p, []byte(`{"mantel": "http://x/", "cast": {"enabled": false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", p}) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the signal: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSharedConfigDetectsAnyMantelKey(t *testing.T) {
	// A tokenless Mantel config with none of the old eight marker keys is
	// still recognised (it used to fail the strict mantel-cast.json decode).
	c, err := parseConfig([]byte(`{"weather": {"lat": 1, "lon": 2}, "alerts": {}, "cast": {"device": "Hub"}}`))
	if err != nil || c.Mantel != "http://auto:8080/" || c.Cast.Device != "Hub" {
		t.Fatalf("Mantel config without markers: %+v %v", c, err)
	}
	if _, err := parseConfig([]byte(`{"mantel": "http://h/", "tokne": "x"}`)); err == nil || !strings.Contains(err.Error(), "mantel-cast.json") {
		t.Fatalf("mantel-cast.json typo should name the mode: %v", err)
	}
}

// mantelPeer is Mantel's config source in a checkout of
// github.com/okchum/mantel next to this repo (CI checks both out side by side).
const mantelPeer = "../mantel/internal/config/config.go"

func TestMantelKeysCoverMantelConfig(t *testing.T) {
	// Every top-level key of Mantel's Config (bar the ones mantel-cast.json
	// shares) must mark a Mantel config.
	if _, err := os.Stat(mantelPeer); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI must check github.com/okchum/mantel out next to this repo: %v", err)
		}
		t.Skip("mantel is not checked out next to mantel-cast")
	}
	keys, err := structJSONKeys(mantelPeer, "Config")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 5 || !slices.Contains(keys, "public_url") || !slices.Contains(keys, "cast") {
		t.Fatalf("Mantel's Config keys look wrong: %q", keys)
	}
	shared := map[string]bool{"mantel": true, "token": true, "timezone": true, "cast": true}
	for _, key := range keys {
		if !shared[key] && !slices.Contains(mantelKeys, key) {
			t.Errorf("mantelKeys lacks Mantel's %q", key)
		}
	}
}

// structJSONKeys lists the json tag names of the fields of struct typeName
// in the Go file at path. Only the tags count, so field types (such as a
// Duration that aliases another package's type) do not matter.
func structJSONKeys(path, typeName string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == typeName {
			st, _ = ts.Type.(*ast.StructType)
		}
		return st == nil
	})
	if st == nil {
		return nil, fmt.Errorf("%s: no struct type %s", path, typeName)
	}
	var keys []string
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			return nil, err
		}
		name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys, nil
}

func TestStructJSONKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.go")
	src := "package config\n\nimport \"example.com/conf\"\n\ntype Duration = conf.Duration\n\n" +
		"type Other struct{ X int `json:\"other\"` }\n\n" +
		"type Config struct {\n\tA string `json:\"a\"` // c\n\tB, C Duration `json:\"b,omitempty\"`\n" +
		"\tD []struct{ E int `json:\"nested\"` } `json:\"d\"`\n\tF int `json:\"-\"`\n\tG int\n}\n"
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := structJSONKeys(p, "Config")
	if err != nil || !slices.Equal(keys, []string{"a", "b", "d"}) {
		t.Fatalf("%q %v", keys, err)
	}
	if _, err := structJSONKeys(p, "Missing"); err == nil {
		t.Fatal("a missing type must fail")
	}
}
func TestTokenWarning(t *testing.T) {
	a, ab := []string{"A"}, []string{"A", "B"}
	for _, c := range []struct {
		name      string
		emptyEnv  []string
		tok, page string
		dashCast  bool
		want      string // substring; "" = no warning
	}{
		{"all set", nil, "x", "http://h/", true, ""},
		{"dashcast, token gone", a, "", "http://h/", true, "the token reads $A, which is unset or empty; the page is opened without a token"},
		{"two gone", ab, "", "http://h/", true, "$A, $B, which are unset"},
		{"URL has its own k", a, "", "http://h/?k=url", true, ""},
		{"auto URL has its own k", a, "", "http://auto:8080/?k=url", false, ""},
		{"URL does not parse", a, "", "http://h/%zz", true, ""},
		{"custom receiver, no Mantel", a, "", "", false, ""},
		{"dashcast, no Mantel", a, "", "", true, ""},
		{"custom receiver with Mantel", a, "", "http://h/", false, "phone sleep reports are read from Mantel without a token"},
		{"mixed literal and $VAR", a, "Bearer ", "http://h/", true, "the page is opened with a token missing that part"},
		{"mixed, custom receiver", a, "Bearer ", "http://h/", false, "phone sleep reports are read from Mantel with a token missing that part"},
	} {
		got := tokenWarning(c.emptyEnv, c.tok, c.page, c.dashCast)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("mantel-cast.json", []byte(`{"mantel": "http://m/", "cast": {"device": "Hub"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func(args ...string) (*Config, error) {
		t.Helper()
		f := newRunFlags()
		if err := f.fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return f.load()
	}
	if c, err := load(); err != nil || c.Mantel != "http://m/" || c.Cast.Device != "Hub" {
		t.Fatalf("default mantel-cast.json: %+v %v", c, err)
	}
	if c, err := load("-config", "other.json"); err != nil || c.Mantel != "" {
		t.Fatalf("-config other.json (missing) should give defaults: %+v %v", c, err)
	}
}
