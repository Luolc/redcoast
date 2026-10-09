package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// TestSignatureVector signs the PutObject example of the AWS Signature Version 4
// documentation (Authenticating Requests: Using the Authorization Header) and expects
// its published signature.
func TestSignatureVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test%24file.text", strings.NewReader("Welcome to Amazon S3."))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	sum := sha256.Sum256([]byte("Welcome to Amazon S3."))
	sign(req, hex.EncodeToString(sum[:]), "us-east-1",
		Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

// fakeS3 accepts or refuses PutObject requests and keeps the accepted objects.
type fakeS3 struct {
	mu          sync.Mutex
	status      int // response status; 0 accepts
	dailyStatus int // response status for keys under daily/; 0 accepts
	objects     map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	switch {
	case err != nil || r.Method != http.MethodPut:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	case r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDTEST/"):
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dailyStatus != 0 && strings.Contains(r.URL.Path, "/daily/") {
		w.WriteHeader(f.dailyStatus)
		_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code></Error>")
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, "<Error><Code>InternalError</Code><AWSAccessKeyId>AKIDTEST</AWSAccessKeyId></Error>")
		return
	}
	f.objects[r.URL.Path] = body
}

func (f *fakeS3) setDailyStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dailyStatus = status
}

func (f *fakeS3) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeS3) object(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.objects {
		keys = append(keys, key)
	}
	return keys
}

// setup opens a synthetic store and a Backup to a fake S3 on a clock starting at
// 2026-10-06 23:00 UTC.
func setup(t *testing.T) (*session.Store, string, *Backup, *fakeS3, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.sqlite")
	store, err := session.Open(context.Background(), path, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	now := time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC)
	b := New(store, Config{Endpoint: server.URL, Region: "us-east-1", Prefix: "redcoast",
		Credentials: Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"},
		Dir:         filepath.Join(dir, "backup"), Client: server.Client(), Now: func() time.Time { return now }})
	return store, path, b, fake, &now
}

func localFiles(t *testing.T, b *Backup) []string {
	t.Helper()
	entries, err := os.ReadDir(b.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestOnceUploads uploads an hourly copy every run and a daily copy on the first run
// of each UTC day, under keys that carry the time, and removes the local snapshot.
func TestOnceUploads(t *testing.T) {
	_, _, b, fake, now := setup(t)
	ctx := context.Background()
	for range 3 {
		if err := b.Once(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}
	want := []string{
		"/redcoast/daily/20261006T230000Z.sqlite.gz",
		"/redcoast/daily/20261007T000000Z.sqlite.gz",
		"/redcoast/hourly/20261006T230000Z.sqlite.gz",
		"/redcoast/hourly/20261007T000000Z.sqlite.gz",
		"/redcoast/hourly/20261007T010000Z.sqlite.gz",
	}
	got := fake.keys()
	if len(got) != len(want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	for _, key := range want {
		if fake.object(key) == nil {
			t.Fatalf("missing %s in %v", key, got)
		}
	}
	if files := localFiles(t, b); len(files) != 0 {
		t.Fatalf("local snapshot left after upload: %v", files)
	}
	if status := b.Status(); !status.LastSuccess.Equal(time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)) || !status.LastFailure.IsZero() {
		t.Fatalf("status %+v", status)
	}
}

// TestOnceFailureKeepsSnapshot keeps the local snapshot and records the failure when
// the upload is refused, without the access key ID the error document echoes; the next
// run clears it and succeeds.
func TestOnceFailureKeepsSnapshot(t *testing.T) {
	_, _, b, fake, now := setup(t)
	ctx := context.Background()
	fake.setStatus(http.StatusPreconditionFailed)
	err := b.Once(ctx)
	if err == nil {
		t.Fatal("refused upload reported as success")
	}
	status := b.Status()
	if !status.LastSuccess.IsZero() || !status.LastFailure.Equal(*now) || !strings.Contains(status.LastError, "HTTP 412 InternalError") || strings.Contains(status.LastError, "AKIDTEST") {
		t.Fatalf("status %+v", status)
	}
	if files := localFiles(t, b); !reflect.DeepEqual(files, []string{"20261006T230000Z.sqlite.gz"}) {
		t.Fatalf("local files after a failed upload: %v", files)
	}
	fake.setStatus(0)
	*now = now.Add(time.Hour)
	if err := b.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if files := localFiles(t, b); len(files) != 0 {
		t.Fatalf("local files after the next run: %v", files)
	}
	if status := b.Status(); !status.LastSuccess.Equal(*now) {
		t.Fatalf("status %+v", status)
	}
}

// TestDailyRetriedNextDay keeps the snapshot whose daily upload failed at 23:00 and
// uploads it on the next day's first run, so that day still gets its daily copy; a
// retry that fails again on the same day takes no second daily snapshot.
func TestDailyRetriedNextDay(t *testing.T) {
	_, _, b, fake, now := setup(t)
	ctx := context.Background()
	fake.setDailyStatus(http.StatusServiceUnavailable)
	if err := b.Once(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 503 SlowDown") {
		t.Fatalf("daily refused: %v", err)
	}
	if status := b.Status(); !status.LastSuccess.Equal(*now) || !status.LastFailure.Equal(*now) {
		t.Fatalf("status %+v", status)
	}
	*now = now.Add(30 * time.Minute)
	if err := b.Once(ctx); err == nil {
		t.Fatal("the retry was refused too")
	}
	if files := localFiles(t, b); !reflect.DeepEqual(files, []string{"daily-20261006T230000Z.sqlite.gz"}) {
		t.Fatalf("local files while the daily copy is pending: %v", files)
	}
	fake.setDailyStatus(0)
	*now = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	if err := b.Once(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"/redcoast/daily/20261006T230000Z.sqlite.gz", "/redcoast/daily/20261007T000000Z.sqlite.gz"} {
		if fake.object(key) == nil {
			t.Fatalf("missing %s in %v", key, fake.keys())
		}
	}
	if got := len(fake.keys()); got != 5 {
		t.Fatalf("keys %v, want three hourly and two daily", fake.keys())
	}
	if files := localFiles(t, b); len(files) != 0 {
		t.Fatalf("local files after the retry: %v", files)
	}
}

// TestDailyRetryFindsKey counts a retried daily upload answered with 412 as done: the
// earlier attempt reached S3 although its answer was lost.
func TestDailyRetryFindsKey(t *testing.T) {
	_, _, b, fake, now := setup(t)
	ctx := context.Background()
	fake.setDailyStatus(http.StatusServiceUnavailable)
	if err := b.Once(ctx); err == nil {
		t.Fatal("daily refused but reported as success")
	}
	fake.setDailyStatus(http.StatusPreconditionFailed)
	*now = now.Add(time.Hour)
	if err := b.Once(ctx); err == nil || !strings.Contains(err.Error(), "daily/20261007T000000Z") {
		t.Fatalf("a fresh daily upload answered 412 must fail: %v", err)
	}
	if files := localFiles(t, b); !reflect.DeepEqual(files, []string{"daily-20261007T000000Z.sqlite.gz"}) {
		t.Fatalf("the retried snapshot should be gone, the fresh one kept: %v", files)
	}
}

// TestRestoreDrill restores the uploaded copy into a fresh directory and checks what
// the plan's restore drill asks for on synthetic data: integrity_check is ok, a session
// credential issued before the backup still binds, and plan_schedule and settings match.
func TestRestoreDrill(t *testing.T) {
	store, path, b, fake, now := setup(t)
	ctx := context.Background()
	issued, err := store.Issue(ctx, "m1", session.LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	period := session.PlanPeriod{Alias: "sample-a", Plan: "max-5x", From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "manual"}
	if err := store.SetPlanSchedule(ctx, period); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("UPDATE settings SET value = '0.7' WHERE key = 'soft'"); err != nil {
		t.Fatal(err)
	}
	if err := b.Once(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(fake.object("/redcoast/hourly/" + now.Format("20060102T150405Z") + ".sqlite.gz")))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "gateway.sqlite")
	if err := os.WriteFile(restoredPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := session.Open(ctx, restoredPath, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	check, err := sql.Open("sqlite", "file:"+restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = check.Close() }()
	var integrity string
	if err := check.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check %q %v", integrity, err)
	}
	if binding, err := restored.Bind(ctx, issued.Token, []string{"sample-a"}); err != nil || binding.SessionID != issued.ID {
		t.Fatalf("credential issued before the backup: %+v %v", binding, err)
	}
	schedule, err := restored.PlanSchedule(ctx, "sample-a")
	if err != nil || len(schedule) != 1 || schedule[0].Plan != "max-5x" || !schedule[0].From.Equal(period.From) {
		t.Fatalf("plan_schedule %+v %v", schedule, err)
	}
	if soft, hard, err := restored.Thresholds(ctx); err != nil || soft != 0.7 || hard != 0.9 {
		t.Fatalf("thresholds %v %v %v", soft, hard, err)
	}
}
