package spawner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests that need neither root nor workerd.

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

var noCgroup = func() (float64, bool) { return 0, false }

var controlEnv = map[string]string{"CONTROL_CERT": "/c/cert.pem", "CONTROL_KEY": "/c/key.pem", "CONTROL_CLIENT_CA": "/c/ca.pem", "CONTROL_ALLOW_CN": "regional-agent"}

func with(base map[string]string, more map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func TestTheSettingsAreReadAndATypoIsAnError(t *testing.T) {
	c, err := ConfigFromEnv(env(controlEnv), noCgroup)
	if err != nil {
		t.Fatal(err)
	}
	if c.Control != "/sockets" || c.State != "/state" || c.RouterUID != 10001 || c.UIDBase != 20000 || c.UIDCount != 1000 || c.BudgetMiB != 1024 || c.MemoryMiB != 256 || c.CPUQuotaPct != 100 ||
		c.Look != 2*time.Second || c.MemoryLook != 250*time.Millisecond || c.RunawayLooks != 5 || c.PidsLimit != 64 || c.Workerd != "/sup/workerd" || c.MaxCustomers != 1000 ||
		!slices.Equal(c.ControlAllow, []string{"regional-agent"}) || c.ControlListen != ":9443" {
		t.Errorf("defaults: %+v", c)
	}
	if c, _ = ConfigFromEnv(env(controlEnv), func() (float64, bool) { return 1024, true }); c.BudgetMiB != 964 {
		t.Errorf("the VM's limit less the 60 MiB of the two supervisors: %v", c.BudgetMiB)
	}
	given := with(controlEnv, map[string]string{"FN_ESTIMATE_MIB": "80", "WD_MEMORY_MIB": "128", "FN_BUDGET_MIB": "200", "CONTROL_ALLOW_CN": " a, b ,,c", "MAX_CUSTOMERS": "5000", "UID_COUNT": "100"})
	if c, err = ConfigFromEnv(env(given), noCgroup); err != nil || c.EstimateMiB != 80 || c.MemoryMiB != 128 || c.BudgetMiB != 200 || !slices.Equal(c.ControlAllow, []string{"a", "b", "c"}) || c.MaxCustomers != 100 {
		t.Errorf("given: %+v %v", c, err)
	}
	for what, bad := range map[string]map[string]string{
		"zero memory":              {"WD_MEMORY_MIB": "0"},
		"not a number":             {"WD_RUNAWAY_LOOKS": "x"},
		"customers in a word":      {"MAX_CUSTOMERS": "many"},
		"root range":               {"UID_BASE": "0"},
		"a system range":           {"UID_BASE": "999"},
		"the router in the range":  {"UID_BASE": "10000", "UID_COUNT": "10"},
		"no router":                {"ROUTER_UID": "0"},
		"no certificate":           {"CONTROL_CERT": ""},
		"no client CA":             {"CONTROL_CLIENT_CA": ""},
		"nobody may call":          {"CONTROL_ALLOW_CN": " , "},
		"no customers":             {"MAX_CUSTOMERS": "0"},
		"no bundle size":           {"MAX_BUNDLE_BYTES": "0"},
		"an old runtime variable?": {"WD_PIDS": "0"},
	} {
		if _, err := ConfigFromEnv(env(with(controlEnv, bad)), noCgroup); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
	// The control API has no plain mode: without the certificates the settings are refused.
	if _, err := ConfigFromEnv(env(nil), noCgroup); err == nil {
		t.Error("no certificates and no allowed names: the spawner must not start")
	}
}

func TestEachCustomerGetsAUserIdOfItsOwnTheRangeEndsAndANumberIsGivenAgainOnlyWhenFree(t *testing.T) {
	a := newUIDAlloc(20000, 2)
	x, createdX, _ := a.alloc("acme")
	y, _, _ := a.alloc("globex")
	x2, createdX2, _ := a.alloc("acme")
	if x != 20000 || y != 20001 || x2 != x || !createdX || createdX2 {
		t.Errorf("uids %d %d %d", x, y, x2)
	}
	if _, _, err := a.alloc("initech"); err == nil {
		t.Error("a third customer in a range of 2")
	}
	a.free("acme", false)
	z, _, err := a.alloc("initech")
	if err != nil || z != 20000 {
		t.Errorf("the freed number: %d %v", z, err)
	}
	// A number kept back (a process of it would not die) is never given again.
	a.free("globex", true)
	if _, _, err := a.alloc("hooli"); err == nil {
		t.Error("a kept-back number was given again")
	}
}

func TestAKeyIsTheCustomerAndTheGeneration(t *testing.T) {
	for key, want := range map[string][2]any{"acme.3": {"acme", uint64(3)}, "a-1.12": {"a-1", uint64(12)}} {
		id, gen, ok := splitKey(key)
		if !ok || id != want[0] || gen != want[1] {
			t.Errorf("%s: %q %d %v", key, id, gen, ok)
		}
		if makeKey(id, gen) != key {
			t.Errorf("makeKey(%q, %d)", id, gen)
		}
	}
	for _, bad := range []string{"acme", ".1", "acme.x", "AC.1", "a/b.1", "acme.-1"} {
		if _, _, ok := splitKey(bad); ok {
			t.Errorf("%q is no key", bad)
		}
	}
}

func TestTheUserIdOfAProcIsTheRealOne(t *testing.T) {
	if got := procUID([]byte("Name:\tx\nUmask:\t0022\nUid:\t20007\t20007\t20007\t20007\nGid:\t1\t1\t1\t1\n")); got != 20007 {
		t.Errorf("uid %d", got)
	}
	if procUID([]byte("garbage")) != -1 {
		t.Error("garbage has no uid")
	}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	cfg, err := ConfigFromEnv(env(with(controlEnv, map[string]string{"MAX_FUNCTIONS": "3", "MAX_BUNDLE_BYTES": "1000", "MAX_SECRETS": "2", "MAX_STATE_BYTES": "5000"})), noCgroup)
	if err != nil {
		t.Fatal(err)
	}
	return newServer(cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func TestADefinitionIsValidatedBeforeAnythingIsStartedAndTheLimitsAreCappedByThePlatform(t *testing.T) {
	s := testServer(t)
	good := func() Definition {
		return Definition{Functions: map[string][]byte{"hello": []byte("x")}, Secrets: map[string]string{"API_KEY": "k"}}
	}
	d := good()
	if e := s.Validate("acme", &d); e != nil {
		t.Fatal(e)
	}
	if d.Limits.MemoryMiB != 256 || d.Limits.CPUQuotaPct != 100 || d.Limits.Pids != 64 {
		t.Errorf("the limits default to the platform's: %+v", d.Limits)
	}
	d = good()
	d.Limits = Limits{MemoryMiB: 64, CPUQuotaPct: 25, Pids: 16}
	if e := s.Validate("acme", &d); e != nil || d.Limits.MemoryMiB != 64 || d.Limits.CPUQuotaPct != 25 {
		t.Errorf("lower limits are accepted: %v %+v", e, d.Limits)
	}
	bad := map[string]func(*string, *Definition){
		"a bad customer":          func(id *string, d *Definition) { *id = "A_b" },
		"a customer with a slash": func(id *string, d *Definition) { *id = "a/b" },
		"no function":             func(id *string, d *Definition) { d.Functions = nil },
		"too many functions": func(id *string, d *Definition) {
			d.Functions = map[string][]byte{"a": {1}, "b": {1}, "c": {1}, "d": {1}}
		},
		"a bad function name":      func(id *string, d *Definition) { d.Functions = map[string][]byte{"../x": {1}} },
		"an empty bundle":          func(id *string, d *Definition) { d.Functions = map[string][]byte{"a": {}} },
		"a bundle too large":       func(id *string, d *Definition) { d.Functions = map[string][]byte{"a": make([]byte, 1001)} },
		"too many secrets":         func(id *string, d *Definition) { d.Secrets = map[string]string{"A": "1", "B": "1", "C": "1"} },
		"a bad secret name":        func(id *string, d *Definition) { d.Secrets = map[string]string{"A B": "1"} },
		"a secret with a NUL":      func(id *string, d *Definition) { d.Secrets = map[string]string{"A": "x\x00y"} },
		"a secret too long":        func(id *string, d *Definition) { d.Secrets = map[string]string{"A": strings.Repeat("x", 8193)} },
		"more memory than allowed": func(id *string, d *Definition) { d.Limits.MemoryMiB = 257 },
		"too little memory":        func(id *string, d *Definition) { d.Limits.MemoryMiB = 8 },
		"more CPU than allowed":    func(id *string, d *Definition) { d.Limits.CPUQuotaPct = 101 },
		"more processes":           func(id *string, d *Definition) { d.Limits.Pids = 65 },
	}
	for what, mutate := range bad {
		id, d := "acme", good()
		mutate(&id, &d)
		if e := s.Validate(id, &d); e == nil {
			t.Errorf("%s: no refusal", what)
		}
	}
}

func controlRequest(s *Server, method, path, body string, cn string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	// The handlers behind the TLS check: the check itself is in allowed(), tested separately and with real certificates in the integration tests.
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/customers/{id}", s.handlePut)
	mux.HandleFunc("DELETE /v1/customers/{id}", s.handleDelete)
	mux.ServeHTTP(rec, req)
	return rec
}

func putBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestThePutRefusesABodyThatIsNotExactlyADefinition(t *testing.T) {
	s := testServer(t)
	b64 := base64.StdEncoding.EncodeToString([]byte("export default () => new Response('x')"))
	fn := map[string]any{"hello": map[string]string{"bundle": b64}}
	for what, body := range map[string]string{
		"not JSON":          `{`,
		"an unknown field":  putBody(t, map[string]any{"functions": fn, "uid": 0}),
		"an unknown limit":  putBody(t, map[string]any{"functions": fn, "limits": map[string]any{"memory_mib": 64, "root": true}}),
		"two values":        putBody(t, map[string]any{"functions": fn}) + `{}`,
		"a bad base64":      putBody(t, map[string]any{"functions": map[string]any{"hello": map[string]string{"bundle": "***"}}}),
		"a url-safe base64": putBody(t, map[string]any{"functions": map[string]any{"hello": map[string]string{"bundle": "-_-_"}}}),
		"no function":       putBody(t, map[string]any{"functions": map[string]any{}}),
		"a bad name":        putBody(t, map[string]any{"functions": map[string]any{"Hello": map[string]string{"bundle": b64}}}),
		"a number secret":   `{"functions":{"hello":{"bundle":"` + b64 + `"}},"secrets":{"A":1}}`,
	} {
		rec := controlRequest(s, "PUT", "/v1/customers/acme", body, "")
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s: %d %s", what, rec.Code, rec.Body)
		}
	}
	if rec := controlRequest(s, "PUT", "/v1/customers/A..B", `{}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad id in the path: %d", rec.Code)
	}
	big := putBody(t, map[string]any{"functions": map[string]any{"hello": map[string]string{"bundle": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 2000))}}})
	if rec := controlRequest(s, "PUT", "/v1/customers/acme", big, ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a bundle over the limit: %d %s", rec.Code, rec.Body)
	}
	if rec := controlRequest(s, "DELETE", "/v1/customers/nobody", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete of a customer that is not there: %d", rec.Code)
	}
	if len(s.List()) != 0 {
		t.Error("a refused put left a customer behind")
	}
}

func TestOnlyACertificateWithAnAllowedNameGetsThroughTheControlHandler(t *testing.T) {
	s := testServer(t)
	called := false
	h := s.allowed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	// No TLS at all (a plain request), and a TLS connection without a client certificate.
	for _, req := range []*http.Request{httptest.NewRequest("GET", "/v1/customers", nil)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || called {
			t.Errorf("a request with no certificate: %d, called=%v", rec.Code, called)
		}
	}
}
