package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

const version = "1.0.0"

// mnasBaseURL builds the base URL for the mnas service from
// MNAS_SCHEME/MNAS_HOST/MNAS_PORT/MNAS_PATH_PREFIX, e.g.
// "https://192.168.0.150/wol" when mnas sits behind an nginx proxy at a
// path prefix.
func mnasBaseURL() string {
	scheme := os.Getenv("MNAS_SCHEME")
	if scheme == "" {
		scheme = "http"
	}
	host := os.Getenv("MNAS_HOST")
	if host == "" {
		host = "mnas"
	}
	port := os.Getenv("MNAS_PORT")
	if port == "" {
		port = "80"
	}
	prefix := strings.TrimSuffix(os.Getenv("MNAS_PATH_PREFIX"), "/")
	return fmt.Sprintf("%s://%s:%s%s", scheme, host, port, prefix)
}

// mnasClient returns an http.Client configured to reach mnas, optionally
// trusting a self-signed certificate via MNAS_CA_FILE or, as a last
// resort, skipping verification via MNAS_INSECURE_SKIP_VERIFY.
func mnasClient() (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	insecure, _ := strconv.ParseBool(os.Getenv("MNAS_INSECURE_SKIP_VERIFY"))
	caFile := os.Getenv("MNAS_CA_FILE")
	if !insecure && caFile == "" {
		return client, nil
	}

	tlsConfig := &tls.Config{}
	if insecure {
		tlsConfig.InsecureSkipVerify = true
	} else {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read MNAS_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("MNAS_CA_FILE %s: no certificates found", caFile)
		}
		tlsConfig.RootCAs = pool
	}
	client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	return client, nil
}

//go:embed templates
var templates embed.FS

//go:embed static
var staticFS embed.FS

type State string

const (
	StateUnknown   State = "unknown"
	StateAwake     State = "awake"
	StateSuspended State = "suspended"
)

func (s State) valid() bool {
	return s == StateUnknown || s == StateAwake || s == StateSuspended
}

func stateFile() string {
	if v := os.Getenv("URR_STATE_FILE"); v != "" {
		return v
	}
	return "./state.json"
}

type stateSnapshot struct {
	State  State  `json:"state"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

func saveState(m *machine) {
	s, reason := m.Get()
	b, _ := json.Marshal(stateSnapshot{State: s, Reason: reason, At: time.Now().UTC().Format(time.RFC3339)})
	f := stateFile()
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		log.Printf("persist state: mkdir: %v", err)
		return
	}
	if err := os.WriteFile(f, b, 0o644); err != nil {
		log.Printf("persist state: %v", err)
	}
}

func loadState(m *machine) bool {
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return false
	}
	var f stateSnapshot
	if json.Unmarshal(b, &f) != nil || !f.State.valid() {
		return false
	}
	m.Set(f.State, f.Reason+" (restored from file)")
	return true
}

// queryMnasStatus asks the mnas service for the current power state of the
// machine it controls ("awake" | "suspended" in the response body).
func queryMnasStatus() (State, error) {
	statusURL := mnasBaseURL() + "/status"
	req, err := http.NewRequest(http.MethodGet, statusURL, nil)
	if err != nil {
		return StateUnknown, err
	}
	if v := os.Getenv("TOKEN"); v != "" {
		req.Header.Set("Authorization", "Bearer "+v)
	}

	client, err := mnasClient()
	if err != nil {
		return StateUnknown, err
	}
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return StateUnknown, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return StateUnknown, fmt.Errorf("mnas status: %s", resp.Status)
	}
	switch State(bytes.TrimSpace(body)) {
	case StateAwake, StateSuspended:
		return State(bytes.TrimSpace(body)), nil
	default:
		return StateUnknown, fmt.Errorf("mnas status: unrecognized body %q", bytes.TrimSpace(body))
	}
}

// machine is a tiny in-memory state machine with a mutex so handler
// goroutines can read/write state concurrently.
type machine struct {
	mu     sync.RWMutex
	state  State
	reason string
}

func (m *machine) Get() (State, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state, m.reason
}

func (m *machine) Set(s State, reason string) {
	m.mu.Lock()
	if m.state != s {
		log.Printf("state: %s -> %s (%s)", m.state, s, reason)
	}
	m.state, m.reason = s, reason
	m.mu.Unlock()
	saveState(m)
}

var current = &machine{state: StateUnknown, reason: "not set"}

type ButtonInfo struct {
	Label     string
	Href      string
	Suspended bool
}

func (s State) button() ButtonInfo {
	if s == StateSuspended {
		return ButtonInfo{Label: "Wake", Href: "/wake", Suspended: true}
	}
	return ButtonInfo{Label: "Suspend", Href: "/suspend"}
}

type pageInfo struct {
	State   string
	Reason  string
	Btn     ButtonInfo
	Version string
}

var pageTmpl = template.Must(template.ParseFS(templates, "templates/index.html"))

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	state, reason := current.Get()
	data := pageInfo{State: string(state), Reason: reason, Btn: state.button(), Version: version}
	if err := pageTmpl.Execute(w, data); err != nil {
		log.Printf("exec template: %v", err)
	}
}

type wakeResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

func runWake() wakeResult {
	name := os.Getenv("URR_CMD")
	if name == "" {
		name = "urr"
	}
	arg := os.Getenv("URR_ARG")
	if arg == "" {
		arg = "mnasx"
	}

	// exec.Command finds the binary in PATH before running it
	cmd := exec.Command(name, arg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := wakeResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		res.ExitCode = 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else {
		res.Error = err.Error()
	}
	return res
}

func suspendHandler(w http.ResponseWriter, r *http.Request) {
	target := mnasBaseURL() + "/suspend"

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if token := os.Getenv("TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client, err := mnasClient()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		current.Set(StateUnknown, "mnas unreachable")
		httpError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		current.Set(StateSuspended, "mnas confirmed suspend")
		w.WriteHeader(http.StatusOK)
	} else {
		current.Set(StateUnknown, "mnas returned "+resp.Status)
		w.WriteHeader(http.StatusBadGateway)
	}

	if ctype := resp.Header.Get("Content-Type"); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func wakeHandler(w http.ResponseWriter, r *http.Request) {
	res := runWake()
	switch {
	case res.Error != "":
		current.Set(StateUnknown, res.Error)
		w.WriteHeader(http.StatusInternalServerError)
	case res.ExitCode != 0:
		current.Set(StateUnknown, fmt.Sprintf("urr exited with %d", res.ExitCode))
		w.WriteHeader(http.StatusBadGateway)
	default:
		current.Set(StateAwake, "wake acknowledged")
		w.WriteHeader(http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func stateHandler(w http.ResponseWriter, r *http.Request) {
	s, reason := current.Get()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"state":  string(s),
		"reason": reason,
		"action": s.button().Href,
	})
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func main() {
	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}

	// Startup: authoritative source is mnas; fall back to the persisted file.
	switch s, err := queryMnasStatus(); {
	case err == nil:
		current.Set(s, "queried from mnas at boot")
	default:
		if loadState(current) {
			log.Printf("boot: mnas unreachable (%v), restored state from %s", err, stateFile())
		} else {
			log.Printf("boot: mnas unreachable (%v), no persisted state, starting unknown", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/state", stateHandler)
	mux.HandleFunc("/suspend", suspendHandler)
	mux.HandleFunc("/wake", wakeHandler)
	mux.HandleFunc("/css/milligram.css", func(w http.ResponseWriter, r *http.Request) {
		b, err := staticFS.ReadFile("static/milligram.css")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write(b)
	})

	bootState, bootReason := current.Get()
	log.Printf("urr-gui v%s listening on %s (state=%s: %s)", version, addr, bootState, bootReason)
	log.Fatal(http.ListenAndServe(addr, mux))
}
