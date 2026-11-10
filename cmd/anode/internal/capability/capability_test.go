package capability

import (
	"context"
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

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// testArtifact 造一份最小可用产物，返回归一化对象、digest 与可下发的 YAML 字节。
func testArtifact(t *testing.T, name string, version int, commands []string) (*artifact.Artifact, string, []byte) {
	t.Helper()
	a := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: name, Version: version},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\necho ok\n"},
			},
			Requires: artifact.Requires{Commands: commands, Values: []string{}},
		},
	}
	data, err := artifact.MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	parsed, digest, err := artifact.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return parsed, digest, data
}

func refFor(digest string) protocol.DeployRefs {
	return protocol.DeployRefs{Namespace: "default", Name: "greeter", Version: 3, Digest: digest}
}

func TestFetchSuccess(t *testing.T) {
	_, digest, data := testArtifact(t, "greeter", 3, []string{"sh"})
	var requests int32
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get(protocol.EmitAuthHeader)
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	fetcher, err := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "node-token")
	if err != nil {
		t.Fatal(err)
	}
	fetcher.AllowNamespace = func(ns string) bool { return ns == "default" }
	a, gotDigest, err := fetcher.Fetch(context.Background(), refFor(digest))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotDigest != digest || a.Metadata.Name != "greeter" || a.Metadata.Version != 3 {
		t.Fatalf("产物 = %+v, digest = %s", a.Metadata, gotDigest)
	}
	if gotPath != ArtifactPathPrefix+digest {
		t.Fatalf("GET path = %q", gotPath)
	}
	if gotAuth != "Bearer node-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if requests != 1 {
		t.Fatalf("请求次数 = %d", requests)
	}
}

func TestFetchRejectsAllCredentialRedirects(t *testing.T) {
	_, digest, data := testArtifact(t, "greeter", 3, nil)
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, route := range []string{"same_origin", "different_port", "different_host", "tls_to_http"} {
			for _, policy := range []string{"default", "permissive"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, route, policy), func(t *testing.T) {
					var firstRequests, secondRequests, redirectCalls atomic.Int32
					var credentialsOK atomic.Bool
					secondHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						secondRequests.Add(1)
						_, _ = w.Write(data)
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
						credentialsOK.Store(r.Method == http.MethodGet && r.Header.Get(protocol.EmitAuthHeader) == "Bearer private-node-token")
						w.Header().Set("Location", target)
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "<html>raw-fetch-body-marker private-node-token</html>")
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
					fetcher, err := NewFetcher(first.URL, "private-node-token")
					if err != nil {
						t.Fatal(err)
					}
					fetcher.SetHTTPClient(caller)
					_, _, err = fetcher.Fetch(context.Background(), refFor(digest))
					var fetchErr *Error
					if !errors.As(err, &fetchErr) || fetchErr.Status != status || fetchErr.Code != CodeBadStatus {
						t.Errorf("redirect must fail with original HTTP %d, got %v", status, err)
					}
					if secondRequests.Load() != 0 {
						t.Errorf("Location target received %d requests; must be zero", secondRequests.Load())
					}
					if firstRequests.Load() != 1 || !credentialsOK.Load() || redirectCalls.Load() != 0 {
						t.Error("initial credential GET or enforced redirect policy violated")
					}
					if fetcher.http != caller || caller.Transport != original.Transport || caller.Timeout != original.Timeout || caller.Jar != original.Jar || reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(original.CheckRedirect).Pointer() {
						t.Error("injected client settings were mutated")
					}
					assertFetchErrorSafe(t, err, "private-node-token", "raw-fetch-body-marker", "<html>")
				})
			}
		}
	}
}

func assertFetchErrorSafe(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	var fetchErr *Error
	if !errors.As(err, &fetchErr) {
		t.Fatalf("expected fetch failure, got %v", err)
	}
	encoded, marshalErr := json.Marshal(fetchErr)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, text := range []string{err.Error(), fmt.Sprint(err), fmt.Errorf("deploy HTTP failure: %w", err).Error(), fmt.Sprintf("%#v", fetchErr), string(encoded)} {
		for _, secret := range forbidden {
			if strings.Contains(text, secret) {
				t.Errorf("HTTP error exposed untrusted response value %q", secret)
				return
			}
		}
	}
}

func TestFetchMalformedLocationDoesNotEchoCredentials(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	for _, policy := range []string{"default", "permissive"} {
		t.Run(policy, func(t *testing.T) {
			var targetRequests, redirectCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) }))
			defer target.Close()
			var credentialsOK atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				credentialsOK.Store(r.Header.Get(protocol.EmitAuthHeader) == "Bearer private-node-token")
				w.Header().Set("Location", target.URL+"/private-node-token/%zz")
				w.WriteHeader(http.StatusFound)
				_, _ = io.WriteString(w, "<html>raw-fetch-body-marker</html>")
			}))
			defer srv.Close()
			caller := srv.Client()
			if policy == "permissive" {
				caller.CheckRedirect = func(*http.Request, []*http.Request) error { redirectCalls.Add(1); return nil }
			}
			original := *caller
			fetcher, err := NewFetcher(srv.URL, "private-node-token")
			if err != nil {
				t.Fatal(err)
			}
			fetcher.SetHTTPClient(caller)
			a, gotDigest, err := fetcher.Fetch(context.Background(), refFor(digest))
			if a != nil || gotDigest != "" || !IsCode(err, CodeNetwork) || !credentialsOK.Load() || targetRequests.Load() != 0 || redirectCalls.Load() != 0 {
				t.Fatalf("malformed Location must fail without forwarding: %v", err)
			}
			if caller.Transport != original.Transport || caller.Timeout != original.Timeout || caller.Jar != original.Jar || reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(original.CheckRedirect).Pointer() {
				t.Error("injected client settings were mutated")
			}
			assertFetchErrorSafe(t, err, "private-node-token", "raw-fetch-body-marker", "%zz")
		})
	}
}

func TestFetchTransportFailureClassification(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	for _, kind := range []string{"timeout", "canceled", "failed"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			fetcher, err := NewFetcher(srv.URL, "private-node-token")
			if err != nil {
				t.Fatal(err)
			}
			fetcher.SetHTTPClient(caller)
			a, gotDigest, err := fetcher.Fetch(ctx, refFor(digest))
			var fetchErr *Error
			if a != nil || gotDigest != "" || !errors.As(err, &fetchErr) || fetchErr.Code != CodeNetwork || fetchErr.Status != 0 || !strings.Contains(strings.ToLower(fetchErr.Message), kind) {
				t.Fatalf("expected classified %s network failure, got %v", kind, err)
			}
			assertFetchErrorSafe(t, err, "private-node-token")
		})
	}
}

func TestFetchStatusErrors(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, CodeBadRef},
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusForbidden, CodeForbidden},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusInternalServerError, CodeBadStatus},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "<html>raw-fetch-body-marker private-node-token</html>", tc.status)
			}))
			defer srv.Close()
			fetcher, err := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "private-node-token")
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = fetcher.Fetch(context.Background(), refFor(digest))
			if err == nil || !IsCode(err, tc.code) {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
			var fetchErr *Error
			if !errors.As(err, &fetchErr) || fetchErr.Status != tc.status {
				t.Fatalf("status = %+v", err)
			}
			assertFetchErrorSafe(t, err, "private-node-token", "raw-fetch-body-marker", "<html>")
		})
	}
}

func TestFetchDigestMismatch(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	_, otherDigest, otherData := testArtifact(t, "greeter", 3, []string{"other"})
	if otherDigest == digest {
		t.Fatal("测试产物 digest 不应相同")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(otherData)
	}))
	defer srv.Close()
	fetcher, _ := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "tok")
	// 用真实 digest 当请求，但服务端回另一份内容。
	_, _, err := fetcher.Fetch(context.Background(), refFor(digest))
	if err == nil || !IsCode(err, CodeDigestMismatch) {
		t.Fatalf("err = %v, want digest_mismatch", err)
	}
}

func TestFetchRefMismatch(t *testing.T) {
	_, digest, data := testArtifact(t, "greeter", 3, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	fetcher, _ := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "tok")
	ref := refFor(digest)
	ref.Name = "other"
	_, _, err := fetcher.Fetch(context.Background(), ref)
	if err == nil || !IsCode(err, CodeRefMismatch) {
		t.Fatalf("err = %v, want ref_mismatch", err)
	}
	ref = refFor(digest)
	ref.Version = 9
	_, _, err = fetcher.Fetch(context.Background(), ref)
	if err == nil || !IsCode(err, CodeRefMismatch) {
		t.Fatalf("version 不符 err = %v, want ref_mismatch", err)
	}
}

func TestFetchInvalidSpec(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "apiVersion: anotify/v1\nkind: Capability\nmetadata: {name: greeter, version: 3}\nspec:\n  run: {entrypoint: nope}\n")
	}))
	defer srv.Close()
	fetcher, _ := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "tok")
	_, _, err := fetcher.Fetch(context.Background(), refFor(digest))
	if err == nil || !IsCode(err, CodeBadArtifact) {
		t.Fatalf("err = %v, want bad_artifact", err)
	}
}

func TestFetchNamespaceDeniedNoRequest(t *testing.T) {
	_, digest, _ := testArtifact(t, "greeter", 3, nil)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
	}))
	defer srv.Close()
	fetcher, _ := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "tok")
	fetcher.AllowNamespace = func(ns string) bool { return ns == "team-a" }
	_, _, err := fetcher.Fetch(context.Background(), refFor(digest))
	if err == nil || !IsCode(err, CodeNamespaceDeny) {
		t.Fatalf("err = %v, want namespace_denied", err)
	}
	if requests != 0 {
		t.Fatalf("未授权 ns 不应发 GET，实际 %d 次", requests)
	}
}

func TestFetchBadRef(t *testing.T) {
	fetcher, _ := NewFetcher("ws://127.0.0.1:1", "tok")
	for _, ref := range []protocol.DeployRefs{
		{Namespace: "default", Name: "greeter", Version: 1, Digest: "sha256:bad"},
		{Namespace: "default", Name: "Greeter", Version: 1, Digest: "sha256:" + strings.Repeat("a", 64)},
		{Namespace: "default", Name: "greeter", Version: 0, Digest: "sha256:" + strings.Repeat("a", 64)},
		{Namespace: "Bad", Name: "greeter", Version: 1, Digest: "sha256:" + strings.Repeat("a", 64)},
	} {
		if _, _, err := fetcher.Fetch(context.Background(), ref); err == nil || !IsCode(err, CodeBadRef) {
			t.Fatalf("ref %+v err = %v, want bad_ref", ref, err)
		}
	}
}

// ---------- cache ----------

func TestCacheStoreLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(filepath.Join(root, "capabilities"))
	a, digest, _ := testArtifact(t, "greeter", 3, []string{"sh"})
	entry, err := cache.Store(a, digest)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	info, err := os.Stat(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("缓存文件权限 = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(entry.Path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("条目目录权限 = %o, want 700", dirInfo.Mode().Perm())
	}
	loaded, err := cache.Load(digest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Digest != digest || loaded.Name != "greeter" || loaded.Version != 3 {
		t.Fatalf("loaded = %+v", loaded)
	}
	entries, problems := cache.Entries()
	if len(problems) != 0 || len(entries) != 1 || entries[0].Digest != digest {
		t.Fatalf("Entries = %+v problems=%v", entries, problems)
	}
}

func TestCacheStoreRejectsDigestMismatch(t *testing.T) {
	cache := NewCache(t.TempDir())
	a, _, _ := testArtifact(t, "greeter", 3, nil)
	other := "sha256:" + strings.Repeat("b", 64)
	if _, err := cache.Store(a, other); err == nil {
		t.Fatal("digest 不一致应拒绝")
	}
}

func TestCacheRejectsCorruptContent(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(root)
	a, digest, _ := testArtifact(t, "greeter", 3, nil)
	if _, err := cache.Store(a, digest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"), artifactFileName)
	if err := os.WriteFile(target, []byte("this is not an artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(digest); err == nil {
		t.Fatal("损坏缓存应拒绝读取")
	}
	entries, problems := cache.Entries()
	if len(entries) != 0 || len(problems) != 1 {
		t.Fatalf("Entries = %+v, problems = %v", entries, problems)
	}
}

func TestCacheRejectsSymlinkEntryDir(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(root)
	a, digest, _ := testArtifact(t, "greeter", 3, nil)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(digest); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接条目应拒绝: %v", err)
	}
	if _, err := cache.Store(a, digest); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接条目不应可写: %v", err)
	}
}

func TestCacheRejectsSymlinkFile(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(root)
	_, digest, data := testArtifact(t, "greeter", 3, nil)
	outside := filepath.Join(t.TempDir(), "artifact.yaml")
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entryDir := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(entryDir, artifactFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(digest); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接文件应拒绝: %v", err)
	}
}

func TestCacheEntriesKeepsGoodSkipsCorrupt(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(root)
	good, digest, _ := testArtifact(t, "greeter", 3, nil)
	if _, err := cache.Store(good, digest); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(root, strings.Repeat("c", 64))
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, artifactFileName), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, problems := cache.Entries()
	if len(entries) != 1 || entries[0].Digest != digest {
		t.Fatalf("好条目应保留: %+v", entries)
	}
	if len(problems) != 1 {
		t.Fatalf("坏条目应进 error 列表: %v", problems)
	}
}

// ---------- deploy ----------

func newDeployFixture(t *testing.T, status int) (*Deployer, *Cache, protocol.CapabilityDeployFrame, string, int32) {
	t.Helper()
	a, digest, data := testArtifact(t, "greeter", 3, []string{"sh"})
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Header.Get(protocol.EmitAuthHeader) != "Bearer node-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "denied", status)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	fetcher, err := NewFetcher("ws"+strings.TrimPrefix(srv.URL, "http"), "node-token")
	if err != nil {
		t.Fatal(err)
	}
	fetcher.AllowNamespace = func(ns string) bool { return ns == "default" }
	cache := NewCache(t.TempDir())
	frame := protocol.CapabilityDeployFrame{
		Type:       protocol.FrameCapabilityDeploy,
		RequestID:  "req-1",
		DeployRefs: protocol.DeployRefs{Namespace: "default", Name: a.Metadata.Name, Version: a.Metadata.Version, Digest: digest},
	}
	return &Deployer{Fetcher: fetcher, Cache: cache}, cache, frame, digest, requests
}

func TestDeploySuccessCachesAndAcks(t *testing.T) {
	deployer, cache, frame, digest, _ := newDeployFixture(t, http.StatusOK)
	ack := deployer.Handle(context.Background(), &frame)
	if ack == nil || ack.Status != protocol.DeployStatusApplied {
		t.Fatalf("ack = %+v", ack)
	}
	if ack.RequestID != frame.RequestID || ack.DeployRefs != frame.DeployRefs {
		t.Fatalf("回执 refs 必须与请求一致: %+v vs %+v", ack.DeployRefs, frame.DeployRefs)
	}
	if ack.Error != "" {
		t.Fatalf("成功回执不应有 error: %q", ack.Error)
	}
	if !cache.Has(digest) {
		t.Fatal("成功后必须已缓存")
	}
}

func TestDeployFailureAckSameRefs(t *testing.T) {
	deployer, cache, frame, digest, _ := newDeployFixture(t, http.StatusForbidden)
	ack := deployer.Handle(context.Background(), &frame)
	if ack == nil || ack.Status != protocol.DeployStatusFailed {
		t.Fatalf("ack = %+v", ack)
	}
	if ack.RequestID != frame.RequestID || ack.DeployRefs != frame.DeployRefs {
		t.Fatalf("失败回执也必须带同一组 refs: %+v", ack)
	}
	if !strings.Contains(ack.Error, "forbidden") {
		t.Fatalf("失败原因 = %q", ack.Error)
	}
	if cache.Has(digest) {
		t.Fatal("失败不应缓存")
	}
}

func TestDeployUnauthorizedNamespaceNoGet(t *testing.T) {
	deployer, _, frame, _, requests := newDeployFixture(t, http.StatusOK)
	frame.Namespace = "team-b"
	ack := deployer.Handle(context.Background(), &frame)
	if ack.Status != protocol.DeployStatusFailed || !strings.Contains(ack.Error, "namespace_denied") {
		t.Fatalf("ack = %+v", ack)
	}
	if atomic.LoadInt32(&requests) != 0 {
		t.Fatal("未授权 ns 不应触发 GET")
	}
}

func TestDeployNilFrame(t *testing.T) {
	deployer := &Deployer{}
	ack := deployer.Handle(context.Background(), nil)
	if ack == nil || ack.Status != protocol.DeployStatusFailed {
		t.Fatalf("ack = %+v", ack)
	}
}
