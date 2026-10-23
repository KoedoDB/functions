package spawner

// The internal socket: the router's door to the spawner. It can do three things and nothing else: say that a request for a function is coming (acquire: the spawner makes sure the
// customer's process is up and returns the socket to use, with a lease), say that the request is over (release), and answer a ping. It cannot create or change a customer, and nothing it
// returns is secret.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
	"github.com/koedodb/functions/internal/wdconfig"
)

// internalSocket is the path of the internal socket.
func (s *Server) internalSocket() string { return filepath.Join(s.cfg.Control, "spawner.sock") }

// internalListener makes the socket (root:RouterGID, 0660) and wraps it: a connection from any user but the router's is closed (SO_PEERCRED), as a second wall behind the permissions of the
// directory.
func (s *Server) internalListener() (net.Listener, error) {
	path := s.internalSocket()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chown(path, 0, s.cfg.RouterGID); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &peerListener{Listener: ln, uid: s.cfg.RouterUID, log: s.log}, nil
}

type peerListener struct {
	net.Listener
	uid int
	log interface{ Warn(string, ...any) }
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			_ = c.Close()
			continue
		}
		cred, err := peerCred(uc)
		if err != nil || int(cred.Uid) != l.uid {
			l.log.Warn("a connection to the internal socket from another user is closed", "error", err)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

func peerCred(c *net.UnixConn) (*syscall.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

type acquireResponse struct {
	Lease     string   `json:"lease"`
	Socket    string   `json:"socket"`
	Functions []string `json:"functions"`
}

func (s *Server) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/acquire", s.handleAcquire)
	mux.HandleFunc("POST /v1/release", s.handleRelease)
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	return http.MaxBytesHandler(mux, 4096)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAcquire(w http.ResponseWriter, r *http.Request) {
	id, fn := r.URL.Query().Get("customer"), r.URL.Query().Get("function")
	if !wdconfig.NameRE.MatchString(id) || !wdconfig.NameRE.MatchString(fn) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad name"})
		return
	}
	var c *customer
	var l *residents.Lease[*WProc]
	var err error
	for try := 0; ; try++ {
		c = s.get(id)
		if c == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such customer"})
			return
		}
		if _, ok := c.fns[fn]; !ok { // nothing is started for a function that is not there
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such function"})
			return
		}
		l, err = s.pool.Acquire(r.Context(), makeKey(id, c.gen))
		if errors.Is(err, errStale) && try < 3 { // a put came between the choice and the start: choose again
			continue
		}
		break
	}
	if err != nil {
		switch {
		case errors.Is(err, residents.ErrBusy):
			w.Header().Set("retry-after", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "busy"})
		case r.Context().Err() != nil:
		default:
			s.log.Error("a customer's process could not be started", "customer", id, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "start"})
		}
		return
	}
	p := l.Handle
	wait := time.NewTimer(s.cfg.StartTimeout + 2*time.Second)
	defer wait.Stop()
	select {
	case <-p.ready:
	case <-r.Context().Done():
		l.Release()
		return
	case <-wait.C:
		s.wl.kill(p)
		l.Release()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "start"})
		return
	}
	if p.readyErr != nil {
		s.log.Error("a customer's process did not start", "customer", id, "error", p.readyErr)
		s.wl.kill(p)
		l.Release()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "start"})
		return
	}
	lid := newLeaseID()
	rec := &lease{l: l, p: p}
	rec.timer = time.AfterFunc(s.cfg.LeaseTTL, func() { s.releaseLease(lid, "expired") })
	s.mu.Lock()
	s.leases[lid] = rec
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, acquireResponse{Lease: lid, Socket: p.Socket, Functions: c.names()})
}

// releaseLease ends a lease. outcome is "ok", or "timeout" / "broken" (the request overran or the process failed it: the process is killed and the next request starts a fresh one), or "expired".
func (s *Server) releaseLease(lid, outcome string) bool {
	s.mu.Lock()
	rec := s.leases[lid]
	delete(s.leases, lid)
	s.mu.Unlock()
	if rec == nil {
		return false
	}
	rec.timer.Stop()
	if outcome == "timeout" || outcome == "broken" {
		rec.p.reason.CompareAndSwap(nil, outcome)
		s.wl.kill(rec.p) // an overrun: the customer's process goes, and with it every request in it
	}
	rec.l.Release()
	return true
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	outcome := r.URL.Query().Get("outcome")
	switch outcome {
	case "ok", "timeout", "broken":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad outcome"})
		return
	}
	if !s.releaseLease(r.URL.Query().Get("lease"), outcome) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such lease"}) // released already (it expired)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
