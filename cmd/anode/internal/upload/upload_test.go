package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

type captured struct {
	method, path, token, attempt, lease, artifactPath, contentType string
	body                                                           []byte
}

func newServer(t *testing.T, handler func(n int, c captured, w http.ResponseWriter)) (*httptest.Server, *[]captured) {
	t.Helper()
	var mu int32
	var records []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := captured{
			method: r.Method, path: r.URL.Path,
			token:        r.Header.Get("Authorization"),
			attempt:      r.Header.Get(protocol.ArtifactHeaderAttempt),
			lease:        r.Header.Get(protocol.ArtifactHeaderLeaseID),
			artifactPath: r.Header.Get(protocol.ArtifactHeaderArtifactPath),
			contentType:  r.Header.Get("Content-Type"),
			body:         body,
		}
		records = append(records, c)
		handler(int(atomic.AddInt32(&mu, 1)), c, w)
	}))
	t.Cleanup(srv.Close)
	return srv, &records
}

func writeCopy(t *testing.T, content string) (string, string, int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return p, hex.EncodeToString(sum[:]), int64(len(content))
}

func okResponse(c captured) (int, map[string]any) {
	sha := strings.TrimPrefix(c.path, "/v1/node/tasks/t1/artifacts/")
	path, _ := protocol.DecodeArtifactPathHeader(c.artifactPath)
	return http.StatusCreated, map[string]any{
		"taskId": "t1", "attempt": 1, "path": path, "sha256": sha, "size": len(c.body),
	}
}

func TestUploadRejectsAllCredentialRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, route := range []string{"same_origin", "different_port", "different_host", "tls_to_http"} {
			for _, policy := range []string{"default", "permissive"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, route, policy), func(t *testing.T) {
					local, sha, size := writeCopy(t, "payload")
					var firstRequests, secondRequests, redirectCalls atomic.Int32
					var credentialsOK atomic.Bool
					secondHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						secondRequests.Add(1)
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(protocol.ArtifactUploadResponse{TaskID: "t1", Attempt: 1, Path: "a", SHA256: sha, Size: size})
					})
					second := httptest.NewServer(secondHandler)
					t.Cleanup(second.Close)
					target := second.URL + "/redirected"
					if route == "different_host" {
						target = strings.Replace(target, "127.0.0.1", "localhost", 1)
					} else if route == "same_origin" {
						target = "/redirected"
					}
					firstHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirected" {
							secondHandler.ServeHTTP(w, r)
							return
						}
						firstRequests.Add(1)
						credentialsOK.Store(r.Method == http.MethodPut &&
							r.Header.Get(protocol.EmitAuthHeader) == "Bearer private-node-token" &&
							r.Header.Get(protocol.ArtifactHeaderTaskToken) == "private-attempt-token")
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Location", target)
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "<html>raw-upload-body-marker private-node-token private-attempt-token</html>")
					})
					var first *httptest.Server
					if route == "tls_to_http" {
						first = httptest.NewTLSServer(firstHandler)
					} else {
						first = httptest.NewServer(firstHandler)
					}
					t.Cleanup(first.Close)
					caller := first.Client()
					caller.Timeout = 3 * time.Second
					caller.Jar, _ = cookiejar.New(nil)
					if policy == "permissive" {
						caller.CheckRedirect = func(*http.Request, []*http.Request) error { redirectCalls.Add(1); return nil }
					}
					original := *caller
					client := &Client{BaseURL: first.URL, NodeToken: "private-node-token", HTTP: caller, Retries: 0}
					_, err := client.Upload(context.Background(), Request{TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: "private-attempt-token", Path: "a", SHA256: sha, Size: size, LocalPath: local})
					var uploadErr *Error
					if !errors.As(err, &uploadErr) || uploadErr.Status != status || uploadErr.Code != "transport" {
						t.Errorf("redirect must fail with original HTTP %d, got %v", status, err)
					}
					if secondRequests.Load() != 0 {
						t.Errorf("Location target received %d requests; must be zero", secondRequests.Load())
					}
					if firstRequests.Load() != 1 || !credentialsOK.Load() || redirectCalls.Load() != 0 {
						t.Error("initial credential PUT or enforced redirect policy violated")
					}
					if client.HTTP != caller || caller.Transport != original.Transport || caller.Timeout != original.Timeout || caller.Jar != original.Jar || reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(original.CheckRedirect).Pointer() {
						t.Error("injected client settings were mutated")
					}
					assertUploadErrorSafe(t, err, "private-node-token", "private-attempt-token", "raw-upload-body-marker", "<html>")
				})
			}
		}
	}
}

// Check both ordinary error propagation and the public error fields.
func assertUploadErrorSafe(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	var uploadErr *Error
	if !errors.As(err, &uploadErr) {
		t.Fatalf("expected upload failure, got %v", err)
	}
	encoded, marshalErr := json.Marshal(uploadErr)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, text := range []string{err.Error(), fmt.Sprint(err), fmt.Errorf("task HTTP failure: %w", err).Error(), fmt.Sprintf("%#v", uploadErr), string(encoded)} {
		for _, secret := range forbidden {
			if strings.Contains(text, secret) {
				t.Errorf("HTTP error exposed untrusted response value %q", secret)
				return
			}
		}
	}
}

func TestUploadHTTPErrorDoesNotEchoBody(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 413, 500} {
		for _, format := range []string{"html", "known_code", "unknown_code"} {
			t.Run(fmt.Sprintf("%d/%s", status, format), func(t *testing.T) {
				local, sha, size := writeCopy(t, "payload")
				body := "<html>raw-upload-body-marker private-node-token private-attempt-token</html>"
				if format != "html" {
					code := protocol.ArtifactErrConflict
					if format == "unknown_code" {
						code = "raw-upload-body-marker private-node-token private-attempt-token"
					}
					encoded, err := json.Marshal(map[string]string{"error": code, "message": body, "reason": body, "details": body})
					if err != nil {
						t.Fatal(err)
					}
					body = string(encoded)
				}
				var credentialsOK atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					credentialsOK.Store(r.Header.Get(protocol.EmitAuthHeader) == "Bearer private-node-token" && r.Header.Get(protocol.ArtifactHeaderTaskToken) == "private-attempt-token")
					_, _ = io.Copy(io.Discard, r.Body)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				client := &Client{BaseURL: srv.URL, NodeToken: "private-node-token", HTTP: srv.Client(), Retries: 0}
				resp, err := client.Upload(context.Background(), Request{TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: "private-attempt-token", Path: "a", SHA256: sha, Size: size, LocalPath: local})
				var uploadErr *Error
				if resp != nil || !errors.As(err, &uploadErr) || uploadErr.Status != status || uploadErr.Message == "" {
					t.Fatalf("expected original HTTP %d failure, got response=%+v err=%v", status, resp, err)
				}
				wantCode := "transport"
				if status == 401 || status == 403 {
					wantCode = "unauthorized"
				} else if format == "known_code" && (status == 400 || status == 409 || status == 413) {
					wantCode = protocol.ArtifactErrConflict
				}
				if uploadErr.Code != wantCode || !credentialsOK.Load() {
					t.Errorf("expected closed code %q and initial credentials", wantCode)
				}
				assertUploadErrorSafe(t, err, "private-node-token", "private-attempt-token", "raw-upload-body-marker", "<html>")
			})
		}
	}
}

func TestUploadMalformedLocationDoesNotEchoCredentials(t *testing.T) {
	for _, policy := range []string{"default", "permissive"} {
		t.Run(policy, func(t *testing.T) {
			local, sha, size := writeCopy(t, "payload")
			var targetRequests, redirectCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) }))
			defer target.Close()
			var credentialsOK atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				credentialsOK.Store(r.Header.Get(protocol.EmitAuthHeader) == "Bearer private-node-token" && r.Header.Get(protocol.ArtifactHeaderTaskToken) == "private-attempt-token")
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", target.URL+"/private-node-token/private-attempt-token/%zz")
				w.WriteHeader(http.StatusFound)
				_, _ = io.WriteString(w, "<html>raw-upload-body-marker</html>")
			}))
			defer srv.Close()
			caller := srv.Client()
			if policy == "permissive" {
				caller.CheckRedirect = func(*http.Request, []*http.Request) error { redirectCalls.Add(1); return nil }
			}
			original := *caller
			client := &Client{BaseURL: srv.URL, NodeToken: "private-node-token", HTTP: caller, Retries: 0}
			resp, err := client.Upload(context.Background(), Request{TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: "private-attempt-token", Path: "a", SHA256: sha, Size: size, LocalPath: local})
			if resp != nil || !IsCode(err, "transport") || !credentialsOK.Load() || targetRequests.Load() != 0 || redirectCalls.Load() != 0 {
				t.Fatalf("malformed Location must fail without forwarding: %v", err)
			}
			if caller.Transport != original.Transport || caller.Timeout != original.Timeout || caller.Jar != original.Jar || reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(original.CheckRedirect).Pointer() {
				t.Error("injected client settings were mutated")
			}
			assertUploadErrorSafe(t, err, "private-node-token", "private-attempt-token", "raw-upload-body-marker", "%zz")
		})
	}
}

func TestUploadTransportFailureClassification(t *testing.T) {
	for _, kind := range []string{"timeout", "canceled", "failed"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if kind == "canceled" {
					cancel()
				}
				<-release
			}))
			defer srv.Close()
			defer close(release)
			if kind == "failed" {
				srv.Close()
			}
			caller := srv.Client()
			caller.Timeout = 25 * time.Millisecond
			local, sha, size := writeCopy(t, "payload")
			client := &Client{BaseURL: srv.URL, NodeToken: "private-node-token", HTTP: caller, Retries: 0}
			resp, err := client.Upload(ctx, Request{TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: "private-attempt-token", Path: "a", SHA256: sha, Size: size, LocalPath: local})
			var uploadErr *Error
			if resp != nil || !errors.As(err, &uploadErr) || uploadErr.Code != "transport" || uploadErr.Status != 0 || !strings.Contains(strings.ToLower(uploadErr.Message), kind) {
				t.Fatalf("expected classified %s transport failure, got %v", kind, err)
			}
			assertUploadErrorSafe(t, err, "private-node-token", "private-attempt-token")
		})
	}
}

func TestUploadErrorCodeClosedSet(t *testing.T) {
	for _, code := range []string{protocol.ArtifactErrPathInvalid, protocol.ArtifactErrHashMismatch, protocol.ArtifactErrTooLarge, protocol.ArtifactErrConflict, "private-node-token", "", "unknown"} {
		body, err := json.Marshal(map[string]string{"error": code, "message": "private-node-token"})
		if err != nil {
			t.Fatal(err)
		}
		want := code
		if code == "private-node-token" || code == "" || code == "unknown" {
			want = "transport"
		}
		if got := errorCode(body); got != want {
			t.Errorf("error code %q became %q, want %q", code, got, want)
		}
	}
	if got := errorCode([]byte("not JSON")); got != "transport" {
		t.Errorf("malformed error body became %q", got)
	}
}

func TestUploadConfirmationMismatchDoesNotEchoCredentials(t *testing.T) {
	const nodeToken = "distinct-node-token"
	const attemptToken = "distinct-attempt-token"
	const taskMarker = "raw-confirmation-task-marker"
	const pathMarker = "raw-confirmation-path-marker"
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		for _, field := range []string{"taskId_and_path", "taskId", "path", "attempt", "sha256", "size"} {
			t.Run(fmt.Sprintf("%d/%s", status, field), func(t *testing.T) {
				srv, records := newServer(t, func(_ int, c captured, w http.ResponseWriter) {
					_, body := okResponse(c)
					switch field {
					case "taskId_and_path":
						body["taskId"] = taskMarker + "-<html>-" + nodeToken
						body["path"] = pathMarker + "-<html>-" + attemptToken
					case "taskId":
						body["taskId"] = taskMarker + "-<html>-" + nodeToken
					case "path":
						body["path"] = pathMarker + "-<html>-" + attemptToken
					case "attempt":
						body["attempt"] = 2
					case "sha256":
						body["sha256"] = strings.Repeat("0", 64)
					case "size":
						body["size"] = len(c.body) + 1
					}
					w.WriteHeader(status)
					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Error(err)
					}
				})
				local, sha, size := writeCopy(t, "payload")
				client := &Client{BaseURL: srv.URL, NodeToken: nodeToken, HTTP: srv.Client(), Retries: 0}
				resp, err := client.Upload(context.Background(), Request{
					TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: attemptToken,
					Path: "artifacts/out/report.md", SHA256: sha, Size: size, LocalPath: local,
				})
				var uploadErr *Error
				if resp != nil || !errors.As(err, &uploadErr) || uploadErr.Status != status || uploadErr.Code != "transport" || uploadErr.Message == "" {
					t.Fatalf("mismatched HTTP %d confirmation must fail, got response=%+v err=%v", status, resp, err)
				}
				if len(*records) != 1 || (*records)[0].token != "Bearer "+nodeToken || (*records)[0].attempt != "1" {
					t.Fatal("expected exactly one credential-bearing upload")
				}
				assertUploadErrorSafe(t, err, nodeToken, attemptToken, taskMarker, pathMarker, "<html>", sha, "artifacts/out/report.md")
			})
		}
	}
}

func TestUploadCreatedAndIdempotentOK(t *testing.T) {
	for _, status := range []int{201, 200} {
		srv, records := newServer(t, func(n int, c captured, w http.ResponseWriter) {
			code, body := okResponse(c)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
			_ = code
		})
		local, sha, size := writeCopy(t, "hello")
		client := &Client{BaseURL: srv.URL, NodeToken: "node-token", HTTP: srv.Client(), Retries: 0}
		resp, err := client.Upload(context.Background(), Request{
			TaskID: "t1", Attempt: 1, LeaseID: "lease-1", AttemptToken: "tok-1",
			Path: "artifacts/out/report.md", SHA256: sha, Size: size, LocalPath: local,
		})
		if err != nil {
			t.Fatalf("status %d: Upload: %v", status, err)
		}
		if resp.TaskID != "t1" || resp.Path != "artifacts/out/report.md" || resp.SHA256 != sha || resp.Size != size {
			t.Fatalf("响应四元组不对：%+v", resp)
		}
		c := (*records)[0]
		if c.method != http.MethodPut ||
			c.path != "/v1/node/tasks/t1/artifacts/"+sha ||
			c.token != "Bearer node-token" ||
			c.attempt != "1" || c.lease != "lease-1" ||
			c.artifactPath != "artifacts%2Fout%2Freport.md" ||
			c.contentType != protocol.ArtifactTransferContentType ||
			string(c.body) != "hello" {
			t.Fatalf("上传请求不对：%+v", c)
		}
	}
}

func TestUploadConflictIsTerminal(t *testing.T) {
	srv, records := newServer(t, func(n int, c captured, w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": protocol.ArtifactErrConflict})
	})
	local, sha, size := writeCopy(t, "x")
	client := &Client{BaseURL: srv.URL, NodeToken: "t", HTTP: srv.Client(), Retries: 2}
	_, err := client.Upload(context.Background(), Request{
		TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok",
		Path: "a", SHA256: sha, Size: size, LocalPath: local,
	})
	if !IsCode(err, protocol.ArtifactErrConflict) {
		t.Fatalf("409 必须是 artifact_conflict：%v", err)
	}
	if len(*records) != 1 {
		t.Fatalf("确定性 409 不能重试：%d 次", len(*records))
	}
}

func TestUploadRetriesTransient5xx(t *testing.T) {
	srv, records := newServer(t, func(n int, c captured, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, body := okResponse(c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	})
	local, sha, size := writeCopy(t, "retry")
	client := &Client{
		BaseURL: srv.URL, NodeToken: "t", HTTP: srv.Client(), Retries: 1,
		Backoff: func(int) time.Duration { return 0 },
	}
	if _, err := client.Upload(context.Background(), Request{
		TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok",
		Path: "a", SHA256: sha, Size: size, LocalPath: local,
	}); err != nil {
		t.Fatalf("5xx 后应幂等重试成功：%v", err)
	}
	if len(*records) != 2 {
		t.Fatalf("应有 2 次请求：%d", len(*records))
	}
}

func TestUploadAuthFailureIsNotRetried(t *testing.T) {
	srv, records := newServer(t, func(n int, c captured, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	local, sha, size := writeCopy(t, "x")
	client := &Client{BaseURL: srv.URL, NodeToken: "t", HTTP: srv.Client(), Retries: 2}
	_, err := client.Upload(context.Background(), Request{
		TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok",
		Path: "a", SHA256: sha, Size: size, LocalPath: local,
	})
	if !IsCode(err, "unauthorized") {
		t.Fatalf("401 应是 unauthorized：%v", err)
	}
	if len(*records) != 1 {
		t.Fatalf("401 不能重试：%d", len(*records))
	}
}

func TestUploadRejectsInvalidInput(t *testing.T) {
	local, sha, size := writeCopy(t, "x")
	client := &Client{BaseURL: "http://127.0.0.1:1", NodeToken: "t", Retries: 0}
	cases := map[string]Request{
		"缺 lease":  {TaskID: "t1", Attempt: 1, AttemptToken: "tok", Path: "a", SHA256: sha, Size: size, LocalPath: local},
		"attempt0": {TaskID: "t1", Attempt: 0, LeaseID: "l", AttemptToken: "tok", Path: "a", SHA256: sha, Size: size, LocalPath: local},
		"路径逃逸":     {TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok", Path: "../a", SHA256: sha, Size: size, LocalPath: local},
		"hash 大写":  {TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok", Path: "a", SHA256: strings.ToUpper(sha), Size: size, LocalPath: local},
		"超上限":      {TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok", Path: "a", SHA256: sha, Size: protocol.MaxArtifactTransferBytes + 1, LocalPath: local},
	}
	for name, req := range cases {
		if _, err := client.Upload(context.Background(), req); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
	// 本地副本大小与清单不一致：不能上传（传输前就拒绝）。
	req := Request{TaskID: "t1", Attempt: 1, LeaseID: "l", AttemptToken: "tok",
		Path: "a", SHA256: sha, Size: size + 1, LocalPath: local}
	if _, err := client.Upload(context.Background(), req); !IsCode(err, protocol.ArtifactErrHashMismatch) {
		t.Fatalf("大小/摘要不一致应拒绝：%v", err)
	}
}
