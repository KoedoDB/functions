package spawner

// The rig of the integration tests: the real spawner (as root, in a container with only its four capabilities), the real router as its own user, a real workerd, and customers' bundles
// put through the real control API with mutual TLS. They need root and a workerd (WORKERD_BIN); without either they are skipped. scripts/test-in-docker.sh runs them the way the Pod does.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/koedodb/functions/internal/router"
)

// TestMain makes the test binary the one executable the rig needs: "child" is what the spawner starts for a customer's process, "router" is the router, "connect" tries to connect to a Unix
// socket (the tests run it as another user).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "child":
			if err := ChildMain(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "child:", err)
				os.Exit(1)
			}
		case "router":
			cfg, err := router.ConfigFromEnv(os.LookupEnv)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			if err := router.New(cfg, nil).Run(ctx); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "connect":
			c, err := net.DialTimeout("unix", os.Args[2], 2*time.Second)
			if err != nil {
				fmt.Println("refused:", err)
				os.Exit(1)
			}
			_ = c.Close()
			fmt.Println("connected")
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func workerdBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WORKERD_BIN"); p != "" {
		return p
	}
	p, err := exec.LookPath("workerd")
	if err != nil {
		t.Skip("workerd is not installed (set WORKERD_BIN)")
	}
	return p
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (and the capabilities of the spawner): run scripts/test-in-docker.sh")
	}
}

// ---- certificates

type pki struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  string
	cert   string
	key    string
	pool   *x509.CertPool
	serial int64
}

func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newPKI(t *testing.T, dir string) *pki {
	t.Helper()
	p := &pki{dir: dir, serial: 10}
	var caPEM []byte
	p.ca, p.caKey, caPEM = newCA(t, "test CA")
	p.caPEM = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(p.caPEM, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	p.pool = x509.NewCertPool()
	p.pool.AddCert(p.ca)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der := p.sign(t, &x509.Certificate{Subject: pkix.Name{CommonName: "spawner"}, DNSNames: []string{"spawner"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, &key.PublicKey, p.ca, p.caKey)
	p.cert, p.key = filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key")
	kb, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(p.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(p.key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return p
}

func (p *pki) sign(t *testing.T, tmpl *x509.Certificate, pub *ecdsa.PublicKey, ca *x509.Certificate, caKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	p.serial++
	tmpl.SerialNumber = big.NewInt(p.serial)
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// client is a client certificate of the given common name signed by the CA of the rig (or by another CA, with rogue).
func (p *pki) client(t *testing.T, cn string, rogue bool) tls.Certificate {
	t.Helper()
	ca, caKey := p.ca, p.caKey
	if rogue {
		ca, caKey, _ = newCA(t, "rogue CA")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der := p.sign(t, &x509.Certificate{Subject: pkix.Name{CommonName: cn}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, &key.PublicKey, ca, caKey)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (p *pki) httpClient(certs []tls.Certificate, maxVersion uint16) *http.Client {
	cfg := &tls.Config{RootCAs: p.pool, Certificates: certs, MinVersion: tls.VersionTLS12}
	if maxVersion != 0 {
		cfg.MaxVersion = maxVersion
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
}

// ---- the rig

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type rig struct {
	t         *testing.T
	s         *Server
	pki       *pki
	base      string
	control   string
	state     string
	ctlURL    string
	routerURL string
	logs      *lockedBuffer
	agent     *http.Client
	cfg       Config
	router    *exec.Cmd
}

const routerUID, routerGID = 10001, 10001

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newRig starts a spawner and a router. env overrides the spawner's settings, routerEnv the router's.
func newRig(t *testing.T, env, routerEnv map[string]string) *rig {
	t.Helper()
	needRoot(t)
	bin := workerdBin(t)
	base, err := os.MkdirTemp("", "fs") // short: a Unix socket's path is 107 bytes at most
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil { // a customer's process must be able to walk down to its own run directory
		t.Fatal(err)
	}
	r := &rig{t: t, base: base, control: filepath.Join(base, "c"), state: filepath.Join(base, "s"), logs: &lockedBuffer{}}
	r.pki = newPKI(t, base)
	port, rport := freePort(t), freePort(t)
	vars := map[string]string{
		"CONTROL_DIR": r.control, "STATE_DIR": r.state, "WORKERD_BIN": bin, "CHILD_BIN": os.Args[0],
		"CONTROL_LISTEN": "127.0.0.1:" + strconv.Itoa(port), "CONTROL_CERT": r.pki.cert, "CONTROL_KEY": r.pki.key, "CONTROL_CLIENT_CA": r.pki.caPEM, "CONTROL_ALLOW_CN": "regional-agent",
		"ROUTER_UID": strconv.Itoa(routerUID), "ROUTER_GID": strconv.Itoa(routerGID), "UID_BASE": "20000",
		"FN_BUDGET_MIB": "4000", "FN_MIN_RESIDENCY_MS": "0", "START_MS": "20000", "WD_LOOK_MS": "200", "WD_MEMORY_LOOK_MS": "50", "WD_RUNAWAY_LOOKS": "5", "WD_MEMORY_MIB": "256",
	}
	for k, v := range env {
		vars[k] = v
	}
	cfg, err := ConfigFromEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok }, func() (float64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cfg
	r.s, err = New(cfg, slog.New(slog.NewTextHandler(r.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the spawner did not stop")
		}
		if r.router != nil && r.router.Process != nil {
			_ = r.router.Process.Kill()
			_ = r.router.Wait()
		}
		if t.Failed() {
			lines := strings.Split(strings.TrimSpace(r.logs.String()), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			t.Logf("the spawner's log (the end):\n%s", strings.Join(lines, "\n"))
		}
	})
	r.ctlURL = "https://127.0.0.1:" + strconv.Itoa(port)
	r.agent = r.pki.httpClient([]tls.Certificate{r.pki.client(t, "regional-agent", false)}, 0)
	eventually(t, "the control API answers", func() bool {
		res, err := r.agent.Get(r.ctlURL + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})

	// The router: its own process, as its own user, from the same binary.
	rvars := []string{"LISTEN=127.0.0.1:" + strconv.Itoa(rport), "CONTROL_DIR=" + r.control, "REQUEST_MS=10000"}
	for k, v := range routerEnv {
		rvars = append(rvars, k+"="+v)
	}
	r.router = exec.Command(os.Args[0], "router")
	r.router.Env = rvars
	r.router.Dir = "/"
	r.router.Stdout, r.router.Stderr = r.logs, r.logs
	r.router.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: routerUID, Gid: routerGID, Groups: []uint32{}}}
	if err := r.router.Start(); err != nil {
		t.Fatal(err)
	}
	r.routerURL = "http://127.0.0.1:" + strconv.Itoa(rport)
	eventually(t, "the router answers", func() bool {
		res, err := http.Get(r.routerURL + "/_healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("not true in time: %s", what)
}

// fn is one function in a put: its JavaScript.
type fns map[string]string

type putOpts struct {
	secrets map[string]string
	limits  map[string]any
	fn      map[string]map[string]any // the limits of a function (heap_mb, cpu_ms, ...): the ones left out are the platform's defaults
}

func (r *rig) putBody(f fns, o putOpts) []byte {
	functions := map[string]any{}
	for name, src := range f {
		fe := map[string]any{"bundle": base64.StdEncoding.EncodeToString([]byte(src))}
		if l, ok := o.fn[name]; ok {
			fe["limits"] = l
		}
		functions[name] = fe
	}
	body := map[string]any{"functions": functions}
	if o.secrets != nil {
		body["secrets"] = o.secrets
	}
	if o.limits != nil {
		body["limits"] = o.limits
	}
	b, _ := json.Marshal(body)
	return b
}

type answer struct {
	status int
	body   string
	header http.Header
}

func (r *rig) put(id string, f fns, o putOpts) answer {
	r.t.Helper()
	req, _ := http.NewRequest("PUT", r.ctlURL+"/v1/customers/"+id, bytes.NewReader(r.putBody(f, o)))
	return r.do(r.agent, req)
}

func (r *rig) mustPut(id string, f fns, o putOpts) {
	r.t.Helper()
	if a := r.put(id, f, o); a.status != 200 {
		r.t.Fatalf("put %s: %d %s", id, a.status, a.body)
	}
}

func (r *rig) del(id string) answer {
	req, _ := http.NewRequest("DELETE", r.ctlURL+"/v1/customers/"+id, nil)
	return r.do(r.agent, req)
}

func (r *rig) do(c *http.Client, req *http.Request) answer {
	r.t.Helper()
	res, err := c.Do(req)
	if err != nil {
		return answer{status: 0, body: err.Error()}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return answer{res.StatusCode, string(b), res.Header}
}

// call is a request to the router (the public side).
func (r *rig) call(path string) answer {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.routerURL+path, nil)
	return r.do(&http.Client{Timeout: 60 * time.Second}, req)
}

func (r *rig) uidOf(id string) int {
	r.t.Helper()
	u, ok := r.s.uids.uid(id)
	if !ok {
		r.t.Fatalf("customer %s has no user id", id)
	}
	return u
}

// workerds are the pids of the live workerd processes that run as uid.
func workerds(uid int) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil || bytes.Contains(b, []byte("\nState:\tZ")) || procUID(b) != uid {
			continue
		}
		if comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm"); strings.TrimSpace(string(comm)) == "workerd" {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}

func (r *rig) onlyWorkerd(id string) int {
	r.t.Helper()
	ps := workerds(r.uidOf(id))
	if len(ps) != 1 {
		r.t.Fatalf("%s runs %d workerd processes, want 1: %v", id, len(ps), ps)
	}
	return ps[0]
}

// asUser runs a command as the user and group uid, with no supplementary groups (what a customer's process is).
func asUser(uid int, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}}
	cmd.Dir = "/"
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const (
	jsHello  = `export default (req) => { const u = new URL(req.url); return new Response('hello ' + u.pathname + u.search); };`
	jsCount  = `let n = 0; export default () => new Response(String(++n));`
	jsLoop   = `export default () => { while (true) {} };`
	jsSlow   = `export default async (req) => { const ms = Number(new URL(req.url).searchParams.get('ms') ?? 3000); await new Promise((r) => setTimeout(r, ms)); return new Response('late'); };`
	jsBomb   = `export default () => { const keep = []; for (;;) { const a = new Uint8Array(16 << 20); a.fill(1); keep.push(a); } };`
	jsSyntax = `export default (req) => { this is not javascript`
	jsNoExp  = `export const x = 1;`
	jsFetch  = `export default async (req) => { const u = new URL(req.url).searchParams.get('u'); try { const r = await fetch(u); return new Response('fetched ' + r.status); } catch (e) { return new Response('refused: ' + e.message); } };`
)

func jsWhoami(id string) string {
	return fmt.Sprintf(`export default (req, env) => Response.json({ id: %q, secret: env.API_KEY ?? null, proc: process.env.API_KEY ?? null, keys: Object.keys(process.env).sort(), ld: process.env.LD_PRELOAD ?? null });`, id)
}

// startStray starts a process of the customer's user that the spawner did not start (what a function that got out of workerd would leave behind).
func startStray(uid int) (*exec.Cmd, error) {
	cmd := exec.Command("sleep", "1000")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}, Setpgid: true}
	return cmd, cmd.Start()
}

func dialUnix(path string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
}

// runDirOf is the run directory of the customer's running process (the only one).
func (r *rig) runDirOf(id string) string {
	r.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(r.state, "run"))
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id+".") {
			found = append(found, filepath.Join(r.state, "run", e.Name()))
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("run directories of %s: %v", id, found)
	}
	return found[0]
}
