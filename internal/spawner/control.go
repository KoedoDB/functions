package spawner

// The control API: how the Regional Agent puts a customer's functions and secrets into the VM. HTTPS with mutual TLS only (TLS 1.3, a client certificate signed by CONTROL_CLIENT_CA whose
// common name is in CONTROL_ALLOW_CN); there is no plain mode and no token. It is the only way a bundle or a secret gets in.
//
//	PUT    /v1/customers/<id>   {"functions": {"<name>": {"bundle": "<base64 of index.mjs>", "limits": {"heap_mb": 256, "cpu_ms": 1000, "wall_ms": 8000, "external_mb": 128, "subrequests": 50, "concurrent_subrequests": 32}}}, "secrets": {"NAME": "value"}, "limits": {"memory_mib": 128, "cpu_quota_pct": 50, "pids": 32}}
//	DELETE /v1/customers/<id>
//	GET    /v1/customers        what the VM holds (never the secrets' values), for the Agent to reconcile after a restart: the state is in memory only
//	GET    /v1/stats            the pool's statistics
//	GET    /healthz

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/koedodb/functions/internal/wdconfig"
)

// putRequest is the body of a put. Unknown fields are an error.
type putRequest struct {
	Functions map[string]struct {
		Bundle string `json:"bundle"`
		Limits *struct {
			HeapMB                uint32 `json:"heap_mb"`
			CPUMs                 uint32 `json:"cpu_ms"`
			WallMs                uint32 `json:"wall_ms"`
			ExternalMB            uint32 `json:"external_mb"`
			Subrequests           uint32 `json:"subrequests"`
			ConcurrentSubrequests uint32 `json:"concurrent_subrequests"`
		} `json:"limits"`
	} `json:"functions"`
	Secrets map[string]string `json:"secrets"`
	Limits  *struct {
		MemoryMiB   float64 `json:"memory_mib"`
		CPUQuotaPct float64 `json:"cpu_quota_pct"`
		Pids        int     `json:"pids"`
	} `json:"limits"`
}

func (s *Server) controlServer() (*http.Server, error) {
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(s.cfg.ControlCA)
	if err != nil {
		return nil, err
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("CONTROL_CLIENT_CA has no certificate")
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.ControlCert, s.cfg.ControlKey)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/customers/{id}", s.handlePut)
	mux.HandleFunc("DELETE /v1/customers/{id}", s.handleDelete)
	mux.HandleFunc("GET /v1/customers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.List()) })
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	return &http.Server{
		Handler:           s.allowed(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // a put is up to a few tens of MiB
		WriteTimeout:      2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		},
	}, nil
}

// allowed lets a request through only when the client certificate's common name is one of the allowed ones (the certificate itself was verified by the TLS handshake).
func (s *Server) allowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || !slices.Contains(s.cfg.ControlAllow, r.TLS.PeerCertificates[0].Subject.CommonName) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !wdconfig.NameRE.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad customer name"})
		return
	}
	select { // a body is held in memory while it is read and decoded: a few at a time
	case s.putSem <- struct{}{}:
		defer func() { <-s.putSem }()
	default:
		w.Header().Set("retry-after", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many puts at once"})
		return
	}
	// The body is base64 inside JSON: a third larger than the bundles it holds.
	max := int64(s.cfg.MaxFunctions)*int64(s.cfg.MaxBundleBytes)*4/3 + int64(s.cfg.MaxSecrets)*(16<<10) + (1 << 20)
	if max > s.cfg.MaxStateBytes*2 {
		max = s.cfg.MaxStateBytes * 2
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req putRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the request is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not a valid definition: " + err.Error()})
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body has more than one JSON value"})
		return
	}
	def := Definition{Functions: map[string][]byte{}, Secrets: req.Secrets, FnLimits: map[string]wdconfig.FnLimits{}}
	for name, f := range req.Functions {
		body, err := base64.StdEncoding.Strict().DecodeString(f.Bundle)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the bundle of " + name + " is not base64"})
			return
		}
		def.Functions[name] = body
		if fl := f.Limits; fl != nil {
			def.FnLimits[name] = wdconfig.FnLimits{HeapMB: fl.HeapMB, CPUMs: fl.CPUMs, WallMs: fl.WallMs, ExternalMB: fl.ExternalMB, Subrequests: fl.Subrequests, ConcurrentSubrequests: fl.ConcurrentSubrequests}
		}
	}
	if req.Limits != nil {
		def.Limits = Limits{MemoryMiB: req.Limits.MemoryMiB, CPUQuotaPct: req.Limits.CPUQuotaPct, Pids: req.Limits.Pids}
	}
	if e := s.Validate(id, &def); e != nil {
		writeJSON(w, e.Status, map[string]any{"error": e.Msg})
		return
	}
	info, e := s.Put(r.Context(), id, def)
	if e != nil {
		writeJSON(w, e.Status, map[string]any{"error": e.Msg, "functions": e.Detail})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !wdconfig.NameRE.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad customer name"})
		return
	}
	if !s.Delete(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such customer"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{"running": st.Resident, "starts": st.Starts, "evictions": st.Evictions, "busy": st.Busy, "waits": st.Waits, "used_mib": st.UsedMiB, "budget_mib": st.BudgetMiB})
}
