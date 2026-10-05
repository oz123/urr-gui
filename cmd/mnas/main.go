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
// The process is only running while the machine is awake, so state resets
// to awake on every boot, which is exactly the right semantics for a WOL
// consumer.
type service struct {
	token   string
	state   string
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
		state:   "awake",
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

	// Launch detached so the HTTP reply is flushed before loginctl
	// suspends the whole machine (which kills this process mid-response).
	// logind performs the suspend asynchronously, so we don't wait for it.
	cmd := exec.Command(s.cmd, s.cmdArgs...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		log.Printf("suspend: start %s: %v", s.cmd, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd.Process.Release()
	log.Printf("suspend: launched %s %s", s.cmd, strings.Join(s.cmdArgs, " "))

	s.state = "suspended"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": "suspend initiated"})
}

func (s *service) statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.state))
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
