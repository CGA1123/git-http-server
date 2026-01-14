package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"path/filepath"
	"strings"
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

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("starting git-http-server on %s, serving %s", addr, absRoot)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

type gitHTTPHandler struct {
	rootDir        string
	gitHTTPBackend string
}

func (h *gitHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract repository path from URL
	// URL format: /<repo>.git/info/refs or /<repo>.git/git-upload-pack etc.
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
	fullRepoPath := filepath.Join(h.rootDir, repoPath)

	// Security: ensure we don't escape the root directory
	if !strings.HasPrefix(fullRepoPath, h.rootDir) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Check if repository exists
	if _, err := os.Stat(fullRepoPath); os.IsNotExist(err) {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
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

func findGitHTTPBackend() (string, error) {
	// Common locations for git-http-backend
	paths := []string{
		"/usr/lib/git-core/git-http-backend",
		"/usr/libexec/git-core/git-http-backend",
		"/usr/local/libexec/git-core/git-http-backend",
		"/opt/homebrew/libexec/git-core/git-http-backend",
		"/usr/local/lib/git-core/git-http-backend",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("git-http-backend not found in common locations")
}
