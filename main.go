package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var requestCounterAtomic int64

func marshalJson(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	s := buf.String()
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

func sendJsonText(w http.ResponseWriter, status int, text string, headers map[string]string) {
	n := atomic.AddInt64(&requestCounterAtomic, 1)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-ID", fmt.Sprintf("req-%08x", n))
	if text != "" {
		h.Set("Content-Length", strconv.Itoa(len(text)))
	}
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	if text != "" {
		w.Write([]byte(text))
	}
}

func sendJson(w http.ResponseWriter, status int, body any, headers map[string]string) {
	var text string
	if body != nil {
		text = marshalJson(body) + "\n"
	}
	sendJsonText(w, status, text, headers)
}

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	return b
}

func authed(r *http.Request) bool {
	if shopToken == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+shopToken
}

type responseTracker struct {
	http.ResponseWriter
	status  int
	written bool
}

func (rt *responseTracker) WriteHeader(code int) {
	if !rt.written {
		rt.written = true
		rt.status = code
	}
	rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
	if !rt.written {
		rt.written = true
		rt.status = http.StatusOK
	}
	return rt.ResponseWriter.Write(b)
}

func appHandler(w http.ResponseWriter, r *http.Request) {
	rt := &responseTracker{ResponseWriter: w, status: 200}
	defer func() {
		if err := recover(); err != nil {
			if !rt.written {
				sendJson(rt, 500, map[string]any{"message": "Internal Server Error"}, nil)
			}
		}
	}()

	var raw []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		raw = readBody(r)
	}

	path := r.URL.Path

	if r.Method == http.MethodGet && path == "/healthz" {
		sendJson(rt, 200, map[string]any{"ok": true}, nil)
		return
	}

	if routeGit(rt, r, raw) {
		return
	}

	if path == "/api/graphql" {
		if !authed(r) {
			sendJson(rt, 401, map[string]string{"message": "Bad credentials"}, nil)
			return
		}
		if r.Method == http.MethodPost {
			handleGraphQL(rt, raw)
			return
		} else if r.Method == http.MethodGet {
			q := r.URL.Query().Get("query")
			if q != "" {
				bodyMap := map[string]any{"query": q}
				bodyBytes := []byte(marshalJson(bodyMap))
				handleGraphQL(rt, bodyBytes)
				return
			}
		}
		sendJson(rt, 404, map[string]string{"message": "Not Found"}, nil)
		return
	}

	if path == "/mcp" {
		if !authed(r) {
			sendJson(rt, 401, map[string]string{"message": "Bad credentials"}, nil)
			return
		}
		if r.Method == http.MethodPost {
			handleMcp(rt, raw)
			return
		}
		sendJson(rt, 405, map[string]string{"message": "Method Not Allowed"}, nil)
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !authed(r) {
			sendJson(rt, 401, map[string]string{"message": "Bad credentials"}, nil)
			return
		}
		routeApi(rt, r, raw)
		return
	}

	sendJson(rt, 404, map[string]string{"message": "Not Found"}, nil)
}

func retryDelay(attempt int) time.Duration {
	exp := attempt
	if exp > 8 {
		exp = 8
	}
	ms := 250 << uint(exp)
	if ms > 2000 {
		ms = 2000
	}
	return time.Duration(ms) * time.Millisecond
}

func register(port int, envURL string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	payload, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return fmt.Errorf("register answered HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(envURL + "/done")
		if err == nil && resp.StatusCode == 200 {
			var body struct {
				Done *bool `json:"done"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Done != nil && *body.Done {
				resp.Body.Close()
				return nil
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		} else if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	portStr := os.Getenv("FORGE_PORT")
	if portStr != "" {
		fmt.Printf("[entry] listening on 127.0.0.1:%s\n", portStr)
		err := http.ListenAndServe("127.0.0.1:"+portStr, http.HandlerFunc(appHandler))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
			os.Exit(1)
		}
		return
	}

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
			os.Exit(1)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)

	server := &http.Server{
		Handler: http.HandlerFunc(appHandler),
	}
	go func() {
		_ = server.Serve(ln)
	}()

	if err := register(port, envURL); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	if err := waitForDone(envURL); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}

	_ = server.Close()
	fmt.Println("[entry] done")
	os.Exit(0)
}
