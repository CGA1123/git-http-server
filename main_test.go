package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHealthCheck(t *testing.T) {
	handler := setupTestHandler(t)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("failed to get /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestRequestValidation(t *testing.T) {
	handler := setupTestHandler(t)
	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{
			name:       "no namespace",
			path:       "/repo.git/info/refs",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "too many segments",
			path:       "/a/b/c.git/info/refs",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty namespace",
			path:       "//repo.git/info/refs",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty repo",
			path:       "/namespace/.git/info/refs",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "not a git path",
			path:       "/namespace/repo/info/refs",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "repo not found on fetch",
			path:       "/namespace/repo.git/info/refs?service=git-upload-pack",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(server.URL + tt.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("got status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestPathTraversal(t *testing.T) {
	root := t.TempDir()

	outsideFile := filepath.Join(root, "..", "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	defer os.Remove(outsideFile)

	handler := setupTestHandlerWithRoot(t, root)
	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		name string
		path string
	}{
		{"dot dot slash", "/ns/../../../secret.git/info/refs"},
		{"leading dot dot", "/../secret.git/info/refs"},
		{"encoded slashes", "/ns/..%2F..%2Fsecret.git/info/refs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(server.URL + tt.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				t.Errorf("got status 200, want error status")
			}
		})
	}
}

func TestGitOperations(t *testing.T) {
	tests := []struct {
		name         string
		setupRepo    bool
		repoPath     string
		fileContent  string
		expectedFile string
	}{
		{
			name:         "clone existing repo",
			setupRepo:    true,
			repoPath:     "namespace/repo.git",
			fileContent:  "hello",
			expectedFile: "README",
		},
		{
			name:         "push creates repo",
			setupRepo:    false,
			repoPath:     "newns/newrepo.git",
			fileContent:  "auto-created",
			expectedFile: "README",
		},
		{
			name:         "push to existing repo",
			setupRepo:    true,
			repoPath:     "org/project.git",
			fileContent:  "pushed",
			expectedFile: "file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			if tt.setupRepo {
				repoPath := filepath.Join(root, tt.repoPath)
				if err := os.MkdirAll(repoPath, 0755); err != nil {
					t.Fatalf("failed to create repo dir: %v", err)
				}
				runGit(t, repoPath, "init", "--bare")
				runGit(t, repoPath, "config", "http.receivepack", "true")
			}

			handler := setupTestHandlerWithRoot(t, root)
			server := httptest.NewServer(handler)
			defer server.Close()

			// Create local repo with a commit
			srcRepo := t.TempDir()
			runGit(t, srcRepo, "init")
			runGit(t, srcRepo, "config", "user.email", "test@test.com")
			runGit(t, srcRepo, "config", "user.name", "Test")
			if err := os.WriteFile(filepath.Join(srcRepo, tt.expectedFile), []byte(tt.fileContent), 0644); err != nil {
				t.Fatalf("failed to write file: %v", err)
			}
			runGit(t, srcRepo, "add", ".")
			runGit(t, srcRepo, "commit", "-m", "test commit")

			// Push to server
			remoteURL := server.URL + "/" + tt.repoPath
			cmd := exec.Command("git", "push", remoteURL, "HEAD:refs/heads/main")
			cmd.Dir = srcRepo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("push failed: %v\n%s", err, out)
			}

			// Clone and verify
			cloneDir := t.TempDir()
			cmd = exec.Command("git", "clone", remoteURL, cloneDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("clone failed: %v\n%s", err, out)
			}

			content, err := os.ReadFile(filepath.Join(cloneDir, tt.expectedFile))
			if err != nil {
				t.Fatalf("failed to read cloned file: %v", err)
			}
			if string(content) != tt.fileContent {
				t.Errorf("got content %q, want %q", content, tt.fileContent)
			}
		})
	}
}

func setupTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return setupTestHandlerWithRoot(t, t.TempDir())
}

func setupTestHandlerWithRoot(t *testing.T, root string) http.Handler {
	t.Helper()

	gitPath, err := findGitHTTPBackend()
	if err != nil {
		t.Fatalf("failed to find git-http-backend: %v", err)
	}

	handler := &gitHTTPHandler{
		rootDir:        root,
		gitHTTPBackend: gitPath,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", handler)

	return loggingMiddleware(mux)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
