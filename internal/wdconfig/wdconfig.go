// Package wdconfig makes the workerd configuration of one customer: a router Worker, a Worker for each function, the secrets as bindings that read the process environment.
package wdconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NameRE is a valid customer or function name.
var NameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// EnvNameRE is a valid secret (variable) name.
var EnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// v8FlagRE is what a V8 flag may look like: the platform sets them (WD_V8_FLAGS), never a customer.
var v8FlagRE = regexp.MustCompile(`^--[a-z0-9][a-z0-9_-]{0,60}(=[A-Za-z0-9_.-]{0,30})?$`)

// SecretEnvPrefix is put before the name of a secret in the environment of the workerd process: a customer's secret called LD_PRELOAD must never be the dynamic loader's variable.
const SecretEnvPrefix = "FNSECRET_"

// routerJS runs first in a customer's workerd: /<function>/<rest> goes to the function's Worker as /<rest>. The names of the bindings are F0, F1, ... (a function's name is not a
// valid identifier), and MAP says which is which.
const routerJS = `const MAP = __MAP__;
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const cut = url.pathname.indexOf('/', 1);
    const name = cut < 0 ? url.pathname.slice(1) : url.pathname.slice(1, cut);
    const binding = Object.hasOwn(MAP, name) ? MAP[name] : undefined;
    if (!binding) return new Response('no such function', { status: 404 });
    url.pathname = cut < 0 ? '/' : url.pathname.slice(cut);
    return env[binding].fetch(new Request(url, request));
  },
};
`

// wrapperJS turns the bundle of a function (export default (request) => Response, or an object with a fetch method: the contract of a function) into a Worker. The bundle is
// imported when the first request comes, not when workerd starts: a bundle with no default export is an exception of that request (500), not the end of the customer's process.
const wrapperJS = `let loading;
function load() {
  return (loading ??= import('bundle').then((mod) => {
    const d = mod.default;
    if (typeof d === 'function') return d;
    if (d && typeof d.fetch === 'function') return (request, env, ctx) => d.fetch(request, env, ctx);
    return null;
  }));
}
export default {
  async fetch(request, env, ctx) {
    let handler;
    try {
      handler = await load();
    } catch (err) {
      console.error('the bundle does not load:', err && err.stack ? err.stack : String(err));
      return new Response('function error', { status: 500, headers: { 'content-type': 'text/plain' } });
    }
    if (!handler) return new Response('the bundle must export default a function (request) => Response, or an object with a fetch method', { status: 500 });
    try {
      const response = await handler(request, env, ctx);
      if (!(response instanceof Response)) throw new TypeError('the function must return a Response');
      return response;
    } catch (err) {
      console.error('function error:', err && err.stack ? err.stack : String(err));
      return new Response('function error', { status: 500, headers: { 'content-type': 'text/plain' } });
    }
  },
};
`

// FnLimits are the limits of one function (one Worker of the customer's process), enforced INSIDE workerd by the KoedoDB patch (Worker.limits of workerd.capnp, doc/limits.md): they stop
// that function and no other. 0 means "no limit" to workerd; the spawner never leaves a field at 0 (it fills in the defaults), so a function without limits cannot be made by accident.
type FnLimits struct {
	HeapMB                uint32 // V8 old generation of the Worker's isolate
	CPUMs                 uint32 // CPU time one request may use
	WallMs                uint32 // wall-clock time one request may take
	ExternalMB            uint32 // ArrayBuffer and typed arrays
	Subrequests           uint32 // per request
	ConcurrentSubrequests uint32 // open at once, over all requests of the Worker
}

// zero reports whether no limit is set at all (then no `limits` is written: workerd's own behaviour).
func (l FnLimits) zero() bool { return l == FnLimits{} }

// capnp is the value of the `limits` field of a Worker.
func (l FnLimits) capnp() string {
	return fmt.Sprintf("(cpuMs = %d, wallMs = %d, heapMb = %d, externalMb = %d, subrequests = %d, concurrentSubrequests = %d)",
		l.CPUMs, l.WallMs, l.HeapMB, l.ExternalMB, l.Subrequests, l.ConcurrentSubrequests)
}

// DefaultV8Flags: TurboFan is off. TurboFan compiles some loops without a check for V8's interrupts, so that TerminateExecution (what the patch's CPU, wall-time and heap limits use to stop a
// function) has no effect on them: `for (;;) new Array(1000).fill(0)` ran for ever with a CPU limit of 1 s (found on 2026-10-24, tests in internal/spawner/fnlimits_test.go). With TurboFan off
// (Ignition, Sparkplug and Maglev stay) the same loop is stopped at the limit. The cost is speed of long-running compute; a request handler is mostly waiting for I/O.
var DefaultV8Flags = []string{"--no-turbofan"}

// Function is one function of a customer.
type Function struct {
	Name   string
	Bundle string // the absolute path of index.mjs
	Limits FnLimits
}

// Spec is what the configuration of one customer's workerd is made from.
type Spec struct {
	Base       string     // the directory of the configuration file: workerd reads "embed" paths relative to it (it does not read an absolute one)
	CompatDate string     // the compatibility date of every function
	Functions  []Function // at least one
	V8Flags    []string   // V8 flags of the process (Config.v8Flags): "--no-turbofan", see DefaultV8Flags
	Secrets    []string   // the names of the variables the functions may read (process.env, and env of the handler); the VALUES are in the environment of the process, as SecretEnvPrefix + name
}

// capnpString is s as a Cap'n Proto text literal. Anything that is not printable ASCII or valid UTF-8 text outside the controls is written as \xNN.
func capnpString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c) // printable ASCII, or a byte of a UTF-8 sequence
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Build is the workerd configuration (Cap'n Proto text) of a customer: a router and a Worker for each function. The listening socket is not in it: the supervisor makes it and hands it
// to workerd as a file descriptor (workerd serve --socket-fd http=3), so the address below is never bound. It is deterministic (names and variables are sorted).
func Build(sp Spec) (string, error) {
	if len(sp.Functions) == 0 {
		return "", errors.New("no function")
	}
	if sp.CompatDate == "" || !filepath.IsAbs(sp.Base) {
		return "", errors.New("the compatibility date and the absolute directory of the configuration are needed")
	}
	fns := append([]Function(nil), sp.Functions...)
	sort.Slice(fns, func(i, j int) bool { return fns[i].Name < fns[j].Name })
	keys := append([]string(nil), sp.Secrets...)
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 && keys[i-1] == k {
			return "", fmt.Errorf("the variable %q is twice in the list", k)
		}
		if !EnvNameRE.MatchString(k) {
			return "", fmt.Errorf("%q is not a variable name", k)
		}
	}
	var mapJS strings.Builder
	mapJS.WriteByte('{')
	for i, f := range fns {
		if !NameRE.MatchString(f.Name) {
			return "", fmt.Errorf("%q is not a function name", f.Name)
		}
		if !filepath.IsAbs(f.Bundle) {
			return "", fmt.Errorf("the bundle of %q must be an absolute path", f.Name)
		}
		if _, err := filepath.Rel(sp.Base, f.Bundle); err != nil {
			return "", fmt.Errorf("the bundle of %q: %w", f.Name, err)
		}
		if i > 0 {
			mapJS.WriteString(", ")
		}
		fmt.Fprintf(&mapJS, "%q: %q", f.Name, "F"+strconv.Itoa(i))
	}
	mapJS.WriteByte('}')

	var b strings.Builder
	b.WriteString("using Workerd = import \"/workerd/workerd.capnp\";\n\nconst config :Workerd.Config = (\n  services = [\n")
	fmt.Fprintf(&b, "    (name = \"router\", worker = (\n      modules = [(name = \"worker\", esModule = %s)],\n      compatibilityDate = %s,\n      bindings = [", capnpString(strings.Replace(routerJS, "__MAP__", mapJS.String(), 1)), capnpString(sp.CompatDate))
	for i, f := range fns {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(name = \"F%d\", service = %s)", i, capnpString("fn-"+f.Name))
	}
	b.WriteString("])),\n")
	for _, f := range fns {
		fmt.Fprintf(&b, "    (name = %s, worker = (\n      modules = [(name = \"worker\", esModule = %s), (name = \"bundle\", esModule = embed %s)],\n      compatibilityDate = %s,\n      compatibilityFlags = [\"nodejs_compat_populate_process_env\"],\n      %sbindings = [",
			capnpString("fn-"+f.Name), capnpString(wrapperJS), capnpString(rel(sp.Base, f.Bundle)), capnpString(sp.CompatDate), limitsField(f.Limits))
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(name = %s, fromEnvironment = %s)", capnpString(k), capnpString(SecretEnvPrefix+k))
		}
		b.WriteString("])),\n")
	}
	b.WriteString("  ],\n")
	if len(sp.V8Flags) > 0 {
		b.WriteString("  v8Flags = [")
		for i, f := range sp.V8Flags {
			if !v8FlagRE.MatchString(f) {
				return "", fmt.Errorf("%q is not a V8 flag", f)
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(capnpString(f))
		}
		b.WriteString("],\n")
	}
	// The address is a placeholder: workerd is started with --socket-fd http=3 and uses the socket it inherited.
	b.WriteString("  sockets = [(name = \"http\", address = \"unix:/nonexistent/never-bound\", http = (), service = \"router\")],\n);\n")
	return b.String(), nil
}

// limitsField is the `limits = (...)` line of a Worker, or nothing when the function has no limits.
func limitsField(l FnLimits) string {
	if l.zero() {
		return ""
	}
	return "limits = " + l.capnp() + ",\n      "
}

// rel is path relative to base (workerd reads "embed" paths relative to the configuration file: "..", symbolic links and relative paths work; an absolute path does not).
func rel(base, path string) string {
	r, _ := filepath.Rel(base, path) // IsAbs of both was checked
	return r
}
