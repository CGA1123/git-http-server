package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
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
		rootDir = flag.String("root", "./repositories", "root directory containing git repositories")
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
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /{$}", handler.handleRepoList)
	mux.HandleFunc("GET /{namespace}/{repo}", handler.handleRepoInfo)
	mux.HandleFunc("/", handler.handleGitBackend)

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

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

func (h *gitHTTPHandler) handleRepoList(w http.ResponseWriter, r *http.Request) {
	repos, err := h.listRepositories()
	if err != nil {
		http.Error(w, "failed to list repositories", http.StatusInternalServerError)
		return
	}

	data := repoListData{
		Repos:   repos,
		BaseURL: "http://" + r.Host,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := repoListTemplate.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func (h *gitHTTPHandler) handleRepoInfo(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	name := r.PathValue("repo")

	// Redirect .git URLs to clean URLs
	if strings.HasSuffix(name, ".git") {
		cleanURL := "/" + namespace + "/" + strings.TrimSuffix(name, ".git")
		http.Redirect(w, r, cleanURL, http.StatusMovedPermanently)
		return
	}

	info, err := h.getRepoInfo(namespace, name, r.Host)
	if err != nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := repoInfoTemplate.Execute(w, info); err != nil {
		log.Printf("template error: %v", err)
	}
}

func (h *gitHTTPHandler) handleGitBackend(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Parse /<namespace>/<repo>[.git]/<path> format
	// Supports both with and without .git suffix
	var repoName string
	var pathInfo string

	if idx := strings.Index(path, ".git/"); idx != -1 {
		repoName = path[1:idx] // strip leading /, exclude .git
		pathInfo = path[idx+4:]
	} else if strings.HasSuffix(path, ".git") {
		repoName = strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".git")
		pathInfo = ""
	} else {
		// Handle URLs without .git: /<namespace>/<repo>/<git-path>
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) < 3 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		repoName = parts[0] + "/" + parts[1]
		pathInfo = "/" + parts[2]
	}

	// Validate format: must be <namespace>/<repo>
	parts := strings.Split(repoName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid repository path: must be <namespace>/<repo>", http.StatusBadRequest)
		return
	}

	// Normalize to .git path for filesystem and git-http-backend
	repoPath := repoName + ".git"

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
			"PATH_INFO=/" + repoPath + pathInfo,
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

type commitInfo struct {
	Hash      string
	Message   string
	Timestamp string
}

type repoInfo struct {
	Namespace    string
	Name         string
	FullPath     string
	LatestCommit string
	CloneURL     string
	Commits      []commitInfo
}

func (h *gitHTTPHandler) listRepositories() ([]repoInfo, error) {
	var repos []repoInfo

	entries, err := os.ReadDir(h.rootDir)
	if err != nil {
		return nil, err
	}

	for _, nsEntry := range entries {
		if !nsEntry.IsDir() {
			continue
		}
		nsPath := filepath.Join(h.rootDir, nsEntry.Name())
		repoEntries, err := os.ReadDir(nsPath)
		if err != nil {
			continue
		}
		for _, repoEntry := range repoEntries {
			if !repoEntry.IsDir() || !strings.HasSuffix(repoEntry.Name(), ".git") {
				continue
			}
			repoName := strings.TrimSuffix(repoEntry.Name(), ".git")
			repos = append(repos, repoInfo{
				Namespace: nsEntry.Name(),
				Name:      repoName,
				FullPath:  nsEntry.Name() + "/" + repoName,
			})
		}
	}

	return repos, nil
}

func (h *gitHTTPHandler) getRepoInfo(namespace, name, host string) (*repoInfo, error) {
	repoPath := filepath.Join(h.rootDir, namespace, name+".git")

	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return nil, err
	}

	info := &repoInfo{
		Namespace: namespace,
		Name:      name,
		FullPath:  namespace + "/" + name,
	}

	// Get clone URL
	scheme := "http"
	if host != "" {
		info.CloneURL = fmt.Sprintf("%s://%s/%s/%s.git", scheme, host, namespace, name)
	}

	// Get latest commit
	cmd := exec.Command("git", "-C", repoPath, "log", "-1", "--format=%H %s", "--all")
	if out, err := cmd.Output(); err == nil {
		info.LatestCommit = strings.TrimSpace(string(out))
	} else {
		info.LatestCommit = "(no commits yet)"
	}

	// Get 10 latest commits on default branch
	cmd = exec.Command("git", "-C", repoPath, "log", "-10", "--format=%H|%s|%cI", "HEAD")
	if out, err := cmd.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "|", 3)
			if len(parts) == 3 {
				info.Commits = append(info.Commits, commitInfo{
					Hash:      parts[0][:7],
					Message:   parts[1],
					Timestamp: parts[2],
				})
			}
		}
	}

	return info, nil
}

type repoListData struct {
	Repos   []repoInfo
	BaseURL string
}

var repoListTemplate = template.Must(template.New("list").Parse(`<!DOCTYPE html>
<html>
<head>
    <title>Git Repositories</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; max-width: 800px; margin: 50px auto; padding: 0 20px; }
        h1 { border-bottom: 1px solid #eee; padding-bottom: 10px; }
        h2 { margin-top: 30px; font-size: 1.1em; color: #333; }
        ul { list-style: none; padding: 0; }
        li { padding: 10px 0; border-bottom: 1px solid #f0f0f0; }
        a { color: #0366d6; text-decoration: none; font-weight: 500; }
        a:hover { text-decoration: underline; }
        .empty { color: #666; font-style: italic; }
        .instructions { background: #f6f8fa; padding: 15px; border-radius: 6px; margin: 20px 0; }
        .instructions h3 { margin-top: 0; font-size: 1em; color: #333; }
        .instructions pre { background: #1f2328; color: #e6edf3; padding: 12px; border-radius: 4px; overflow-x: auto; font-size: 0.9em; }
        .instructions code { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace; }
        .instructions p { margin: 10px 0; color: #555; font-size: 0.95em; }
    </style>
</head>
<body>
    <h1>Repositories</h1>
    {{if .Repos}}
    <ul>
        {{range .Repos}}
        <li><a href="/{{.FullPath}}">{{.FullPath}}</a></li>
        {{end}}
    </ul>
    {{else}}
    <p class="empty">No repositories found.</p>
    {{end}}

    <h2>Getting Started</h2>
    <div class="instructions">
        <h3>Push an existing repository</h3>
        <p>Add this server as a remote and push:</p>
        <pre><code>git remote add origin {{.BaseURL}}/&lt;namespace&gt;/&lt;repo&gt;.git
git push -u origin main</code></pre>
    </div>
    <div class="instructions">
        <h3>Create a new repository</h3>
        <p>Initialize a new Git repository and push to create it on this server:</p>
        <pre><code>mkdir my-project && cd my-project
git init
git add .
git commit -m "Initial commit"
git remote add origin {{.BaseURL}}/&lt;namespace&gt;/&lt;repo&gt;.git
git push -u origin main</code></pre>
    </div>
</body>
</html>
`))

var repoInfoTemplate = template.Must(template.New("info").Parse(`<!DOCTYPE html>
<html>
<head>
    <title>{{.FullPath}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; max-width: 800px; margin: 50px auto; padding: 0 20px; }
        h1 { border-bottom: 1px solid #eee; padding-bottom: 10px; }
        h2 { margin-top: 30px; font-size: 1.2em; color: #333; }
        .section { margin: 20px 0; }
        .label { font-weight: 600; color: #333; }
        .value { font-family: monospace; background: #f6f8fa; padding: 8px 12px; border-radius: 4px; display: block; margin-top: 5px; word-break: break-all; }
        a { color: #0366d6; text-decoration: none; }
        a:hover { text-decoration: underline; }
        .commits { list-style: none; padding: 0; }
        .commits li { padding: 10px 0; border-bottom: 1px solid #f0f0f0; display: flex; align-items: center; }
        .commit-hash { font-family: monospace; background: #f6f8fa; padding: 2px 6px; border-radius: 3px; font-size: 0.9em; flex-shrink: 0; }
        .commit-message { margin-left: 10px; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
        .commit-time { color: #666; font-size: 0.85em; margin-left: 10px; flex-shrink: 0; text-align: right; }
    </style>
</head>
<body>
    <p><a href="/">&larr; Back to repositories</a></p>
    <h1>{{.FullPath}}</h1>
    <div class="section">
        <span class="label">Clone URL:</span>
        <code class="value">git clone {{.CloneURL}}</code>
    </div>
    <h2>Recent Commits</h2>
    {{if .Commits}}
    <ul class="commits">
        {{range .Commits}}
        <li>
            <span class="commit-hash">{{.Hash}}</span>
            <span class="commit-message" title="{{.Message}}">{{.Message}}</span>
            <span class="commit-time">{{.Timestamp}}</span>
        </li>
        {{end}}
    </ul>
    {{else}}
    <p style="color: #666; font-style: italic;">No commits yet.</p>
    {{end}}
</body>
</html>
`))

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
