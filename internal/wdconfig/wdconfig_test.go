package wdconfig

import (
	"strings"
	"testing"
)

func TestCapnpStringEscapesWhatBreaksALiteral(t *testing.T) {
	cases := map[string]string{
		`plain`:      `"plain"`,
		`a "quote"`:  `"a \"quote\""`,
		`back\slash`: `"back\\slash"`,
		"two\nlines": `"two\nlines"`,
		"tab\there":  `"tab\there"`,
		"bell\x07":   `"bell\x07"`,
		"del\x7f":    `"del\x7f"`,
		"日本語":        `"日本語"`,
		"cr\rlf":     `"cr\rlf"`,
	}
	for in, want := range cases {
		if got := capnpString(in); got != want {
			t.Errorf("capnpString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTheConfigHasARouterAWorkerForEachFunctionAndTheSecretsAsEnvironmentBindings(t *testing.T) {
	cfg, err := Build(Spec{
		Base: "/state/run/acme.1", CompatDate: "2026-10-01",
		Functions: []Function{{"zeta", "/state/run/acme.1/zeta/index.mjs"}, {"alpha", "/state/run/acme.1/alpha/index.mjs"}},
		Secrets:   []string{"B_KEY", "A_KEY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "router"`, `name = "fn-alpha"`, `name = "fn-zeta"`,
		`embed "alpha/index.mjs"`, `embed "zeta/index.mjs"`, // relative to the directory of the configuration: workerd does not read an absolute embed path
		`service = "router"`,
		`compatibilityFlags = ["nodejs_compat_populate_process_env"]`,
		`(name = "A_KEY", fromEnvironment = "FNSECRET_A_KEY")`, `(name = "B_KEY", fromEnvironment = "FNSECRET_B_KEY")`,
		`(name = "F0", service = "fn-alpha"), (name = "F1", service = "fn-zeta")`, // sorted: alpha is F0
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the configuration lacks %s:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "text =") {
		t.Error("a value is in the configuration: secrets are read from the environment of the process, never written")
	}
	if strings.Index(cfg, `"A_KEY"`) > strings.Index(cfg, `"B_KEY"`) {
		t.Error("the variables are not sorted")
	}
	// No socket of its own: the supervisor hands one down (--socket-fd), so nothing is ever bound by name.
	if !strings.Contains(cfg, `unix:/nonexistent/never-bound`) || strings.Contains(cfg, "/state/run/acme.1.sock") {
		t.Error("the socket address must be the placeholder")
	}
	again, _ := Build(Spec{Base: "/state/run/acme.1", CompatDate: "2026-10-01",
		Functions: []Function{{"alpha", "/state/run/acme.1/alpha/index.mjs"}, {"zeta", "/state/run/acme.1/zeta/index.mjs"}}, Secrets: []string{"A_KEY", "B_KEY"}})
	if again != cfg {
		t.Error("the same input gives another configuration")
	}
}

func TestTheConfigRefusesWhatItCannotMakeSafely(t *testing.T) {
	ok := Spec{Base: "/s/x", CompatDate: "2026-10-01", Functions: []Function{{"a", "/s/x/a/index.mjs"}}}
	bad := map[string]func(*Spec){
		"no function":             func(s *Spec) { s.Functions = nil },
		"no date":                 func(s *Spec) { s.CompatDate = "" },
		"a relative base":         func(s *Spec) { s.Base = "s/x" },
		"a bad name":              func(s *Spec) { s.Functions = []Function{{"A_b", "/s/x/x/index.mjs"}} },
		"a relative bundle":       func(s *Spec) { s.Functions = []Function{{"a", "f/a/index.mjs"}} },
		"a bad variable":          func(s *Spec) { s.Secrets = []string{"A B"} },
		"a variable that is code": func(s *Spec) { s.Secrets = []string{`X", text = "y"), (name = "Z`} },
		"a variable twice":        func(s *Spec) { s.Secrets = []string{"A", "A"} },
	}
	for what, mutate := range bad {
		sp := ok
		mutate(&sp)
		if _, err := Build(sp); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
}

func TestNamesAreSmallLettersDigitsAndDashesOnly(t *testing.T) {
	for _, good := range []string{"a", "acme", "a-b-1", strings.Repeat("a", 40)} {
		if !NameRE.MatchString(good) {
			t.Errorf("%q should be a name", good)
		}
	}
	for _, bad := range []string{"", "A", "a_b", "a.b", "a/b", "..", "a b", strings.Repeat("a", 41), "a\n", "%61"} {
		if NameRE.MatchString(bad) {
			t.Errorf("%q should not be a name", bad)
		}
	}
}
