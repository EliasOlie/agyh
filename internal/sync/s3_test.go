package sync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3ClientCRUD(t *testing.T) {
	storage := make(map[string][]byte)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=test-ak/") {
			t.Errorf("missing or invalid Authorization header: %s", auth)
		}
		if r.Header.Get("x-amz-date") == "" {
			t.Errorf("missing x-amz-date")
		}
		if r.Header.Get("x-amz-content-sha256") == "" {
			t.Errorf("missing x-amz-content-sha256")
		}

		key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			storage[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := storage[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodHead:
			_, ok := storage[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(storage, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	client, err := NewS3Client(S3Config{
		Endpoint:  ts.URL,
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-ak",
		SecretKey: "test-sk",
	})
	if err != nil {
		t.Fatalf("NewS3Client failed: %v", err)
	}

	ctx := context.Background()

	// 1. Head on non-existing object
	exists, err := client.Head(ctx, "file.txt")
	if err != nil {
		t.Fatalf("Head failed: %v", err)
	}
	if exists {
		t.Errorf("expected exists=false")
	}

	// 2. Put
	content := []byte("hello from s3")
	if err := client.Put(ctx, "file.txt", content); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// 3. Head on existing object
	exists, err = client.Head(ctx, "file.txt")
	if err != nil {
		t.Fatalf("Head failed: %v", err)
	}
	if !exists {
		t.Errorf("expected exists=true")
	}

	// 4. Get
	got, err := client.Get(ctx, "file.txt")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}

	// 5. Delete
	if err := client.Delete(ctx, "file.txt"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// 6. Get after delete should return ErrObjectNotFound
	_, err = client.Get(ctx, "file.txt")
	if err != ErrObjectNotFound {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}
