package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		port    = flag.Int("port", 9418, "port to listen on")
		rootDir = flag.String("root", ".", "root directory containing git repositories")
	)
	flag.Parse()

	absRoot, err := filepath.Abs(*rootDir)
	if err != nil {
		log.Fatalf("failed to resolve root directory: %v", err)
	}

	if _, err := os.Stat(absRoot); os.IsNotExist(err) {
		log.Fatalf("root directory does not exist: %s", absRoot)
	}

	gitPath, err := findGitHTTPBackend()
	if err != nil {
		log.Fatalf("failed to find git-http-backend: %v", err)
	}

	handler := &gitHTTPHandler{
		rootDir:        absRoot,
		gitHTTPBackend: gitPath,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", handler)

	addr := fmt.Sprintf(":%d", *port)
	server := &http.Server{
		Addr:    addr,
		Handler: loggingMiddleware(mux),
	}

	// Handle shutdown signals
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		close(done)
	}()

	log.Printf("starting git-http-server on %s, serving %s", addr, absRoot)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}

	<-done
	log.Println("server stopped")
}

type gitHTTPHandler struct {
	rootDir        string
	gitHTTPBackend string
}

func (h *gitHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract repository path from URL
	// URL format: /<namespace>/<repo>.git/info/refs or /<namespace>/<repo>.git/git-upload-pack etc.
	path := r.URL.Path

	// Find the .git part of the path to determine repo boundaries
	var repoPath string
	var pathInfo string

	if idx := strings.Index(path, ".git/"); idx != -1 {
		repoPath = path[:idx+4] // include .git
		pathInfo = path[idx+4:]
	} else if strings.HasSuffix(path, ".git") {
		repoPath = path
		pathInfo = ""
	} else {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Clean and validate the repo path
	repoPath = strings.TrimPrefix(repoPath, "/")

	// Validate format: must be <namespace>/<repo>.git
	repoName := strings.TrimSuffix(repoPath, ".git")
	parts := strings.Split(repoName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid repository path: must be <namespace>/<repo>.git", http.StatusBadRequest)
		return
	}

	fullRepoPath := filepath.Join(h.rootDir, repoPath)

	// Security: ensure we don't escape the root directory
	if !strings.HasPrefix(fullRepoPath, h.rootDir) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Check if repository exists
	if _, err := os.Stat(fullRepoPath); os.IsNotExist(err) {
		// Auto-create repository on push
		if h.isPushRequest(r, pathInfo) {
			if err := h.initRepo(fullRepoPath); err != nil {
				log.Printf("failed to create repository %s: %v", fullRepoPath, err)
				http.Error(w, "failed to create repository", http.StatusInternalServerError)
				return
			}
			log.Printf("created repository: %s", fullRepoPath)
		} else {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
	}

	// Set up CGI handler for git-http-backend
	cgiHandler := &cgi.Handler{
		Path: h.gitHTTPBackend,
		Dir:  fullRepoPath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + h.rootDir,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO=" + "/" + repoPath + pathInfo,
		},
	}

	cgiHandler.ServeHTTP(w, r)
}

func (h *gitHTTPHandler) isPushRequest(r *http.Request, pathInfo string) bool {
	// Check if this is a git-receive-pack request (push)
	if strings.Contains(pathInfo, "git-receive-pack") {
		return true
	}
	// Check query parameter for service=git-receive-pack (info/refs?service=git-receive-pack)
	if r.URL.Query().Get("service") == "git-receive-pack" {
		return true
	}
	return false
}

func (h *gitHTTPHandler) initRepo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	cmd := exec.Command("git", "init", "--bare", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init failed: %w: %s", err, out)
	}

	// Enable push support
	cmd = exec.Command("git", "-C", path, "config", "http.receivepack", "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config failed: %w: %s", err, out)
	}

	// Set default branch to main
	cmd = exec.Command("git", "-C", path, "symbolic-ref", "HEAD", "refs/heads/main")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git symbolic-ref failed: %w: %s", err, out)
	}

	return nil
}

func findGitHTTPBackend() (string, error) {
	cmd := exec.Command("git", "--exec-path")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run 'git --exec-path': %w", err)
	}

	execPath := strings.TrimSpace(string(out))
	backendPath := filepath.Join(execPath, "git-http-backend")

	if _, err := os.Stat(backendPath); err != nil {
		return "", fmt.Errorf("git-http-backend not found at %s: %w", backendPath, err)
	}

	return backendPath, nil
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}
