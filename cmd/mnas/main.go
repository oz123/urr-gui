package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const version = "1.0.0"

// mnas is a minimal service on the target machine:
//
//	GET/POST /suspend  token-gated, runs `loginctl suspend`
//	GET     /status    plain-text "awake" | "suspended"
//
// /status always reports "awake": this process can only ever be scheduled
// to answer an HTTP request while the machine is actually running, so
// there is nothing else it could honestly say. A one-way "suspended" flag
// set on /suspend and never reset doesn't work: `loginctl suspend` is a
// RAM-sleep, so this process's memory - including any such flag - survives
// a suspend/resume cycle untouched, and would keep lying "suspended"
// forever after the machine wakes back up.
type service struct {
	token   string
	cmd     string
	cmdArgs []string
}

func newService() *service {
	cmd := os.Getenv("SUSPEND_CMD")
	if cmd == "" {
		cmd = "loginctl"
	}
	args := []string{"suspend"}
	if v := os.Getenv("SUSPEND_ARGS"); v != "" {
		args = splitArgs(v)
	}
	return &service{
		token:   os.Getenv("TOKEN"),
		cmd:     cmd,
		cmdArgs: args,
	}
}

func splitArgs(v string) []string {
	var out []string
	var cur bytes.Buffer
	for _, r := range v {
		if r == ' ' || r == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func (s *service) authorize(r *http.Request) bool {
	if s.token == "" {
		log.Printf("rejecting /suspend: TOKEN not configured")
		return false
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	got := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *service) suspendHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// loginctl's Suspend D-Bus call returns as soon as logind has accepted
	// the request, well before the machine actually sleeps, so waiting for
	// it is safe and lets us see whether it was accepted at all (wrong
	// privileges, no session, an inhibitor lock, etc. all surface here
	// instead of being silently swallowed).
	cmd := exec.Command(s.cmd, s.cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	w.Header().Set("Content-Type", "application/json")
	if err := cmd.Run(); err != nil {
		log.Printf("suspend: %s %s: %v (stderr: %s)", s.cmd, strings.Join(s.cmdArgs, " "), err, stderr.String())
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"error":  err.Error(),
			"stderr": strings.TrimSpace(stderr.String()),
		})
		return
	}

	log.Printf("suspend: %s %s accepted", s.cmd, strings.Join(s.cmdArgs, " "))
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": "suspend initiated"})
}

func (s *service) statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("awake"))
}

func (s *service) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version})
}

func main() {
	svc := newService()

	addr := ":80"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/suspend", svc.suspendHandler)
	mux.HandleFunc("/status", svc.statusHandler)
	mux.HandleFunc("/health", svc.healthHandler)

	// Short read timeout is fine: all responses are tiny.
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	log.Printf("mnas v%s listening on %s (suspend=%s %s, token=%v)",
		version, addr, svc.cmd, svc.cmdArgs, svc.token != "")
	log.Fatal(srv.ListenAndServe())
}
