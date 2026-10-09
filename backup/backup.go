// Package backup copies the gateway's SQLite file to S3 every hour. Each run writes a
// consistent snapshot with VACUUM INTO, compresses it and uploads it under a key that
// carries the UTC time; the first upload of each UTC day goes to the daily prefix too.
// The bucket's lifecycle rules expire old copies; the gateway never deletes one.
package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Interval is how often Run takes a backup.
const Interval = time.Hour

// uploadTimeout bounds one run's uploads.
const uploadTimeout = 10 * time.Minute

// Snapshotter writes a consistent copy of the database to a path that does not exist.
type Snapshotter interface {
	VacuumInto(ctx context.Context, path string) error
}

// Config says where backups go.
type Config struct {
	Endpoint    string // bucket URL, path-style, e.g. https://s3.<region>.amazonaws.com/<bucket>
	Region      string
	Prefix      string // key prefix without a trailing slash, e.g. redcoast
	Credentials Credentials
	Dir         string           // private directory for the local snapshot
	Client      *http.Client     // nil uses a client that ignores proxy variables
	Now         func() time.Time // nil uses time.Now
}

// Status is what the status command and the health endpoint read. Zero times mean no
// such run since the gateway started.
type Status struct {
	LastSuccess time.Time // the last run whose hourly upload succeeded
	LastFailure time.Time
	LastError   string // the error of the last failed run
}

// Backup runs the hourly backups. Its methods are safe for concurrent use.
type Backup struct {
	store Snapshotter
	cfg   Config

	mu        sync.Mutex
	status    Status
	dailyDone string // the UTC date whose daily copy is uploaded
}

// New returns a Backup of store with cfg.
func New(store Snapshotter, cfg Config) *Backup {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Transport: &http.Transport{}}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Backup{store: store, cfg: cfg}
}

// Status returns the outcome of the latest runs.
func (b *Backup) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// Run backs up once at start and then every Interval until ctx is canceled. A failed
// run is logged and recorded in Status; it never stops the gateway.
func (b *Backup) Run(ctx context.Context) error {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		if err := b.Once(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backup: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Once takes one backup. The local snapshot is removed after every upload succeeded;
// after a failed hourly upload it stays until the next run, which starts by clearing
// the directory. A snapshot whose daily upload failed is kept as daily-<name> and
// retried at the start of every run until it is uploaded, so a day keeps its daily
// copy even when the retry falls on the next day. A retry answered with 412 counts as
// uploaded: only this gateway writes that key, so an earlier attempt got through even
// though its answer was lost.
func (b *Backup) Once(ctx context.Context) error {
	now := b.cfg.Now().UTC()
	err := b.once(ctx, now)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.status.LastFailure, b.status.LastError = now, err.Error()
	}
	return err
}

func (b *Backup) once(ctx context.Context, now time.Time) error {
	pending, err := clearDir(b.cfg.Dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	today := now.Format(time.DateOnly)
	var errs []error
	pendingToday := false
	for _, name := range pending {
		path := filepath.Join(b.cfg.Dir, dailyPending+name)
		if err := b.put(ctx, "daily/"+name, path, now); err != nil && !errors.Is(err, errExists) {
			errs = append(errs, err)
			pendingToday = pendingToday || strings.HasPrefix(name, now.Format("20060102"))
			continue
		}
		if err := os.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("backup: remove snapshot: %v", err))
		}
		if strings.HasPrefix(name, now.Format("20060102")) {
			b.setDailyDone(today)
		}
	}
	name := now.Format("20060102T150405Z") + ".sqlite.gz"
	compressed := filepath.Join(b.cfg.Dir, name)
	if err := b.snapshot(ctx, compressed); err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := b.put(ctx, "hourly/"+name, compressed, now); err != nil {
		return errors.Join(append(errs, err)...)
	}
	b.mu.Lock()
	b.status.LastSuccess = now
	dailyDone := b.dailyDone == today
	b.mu.Unlock()
	if dailyDone || pendingToday {
		if err := os.Remove(compressed); err != nil {
			errs = append(errs, fmt.Errorf("backup: remove snapshot: %v", err))
		}
		return errors.Join(errs...)
	}
	if err := b.put(ctx, "daily/"+name, compressed, now); err != nil {
		if renameErr := os.Rename(compressed, filepath.Join(b.cfg.Dir, dailyPending+name)); renameErr != nil {
			err = errors.Join(err, fmt.Errorf("backup: keep snapshot for the daily copy: %v", renameErr))
		}
		return errors.Join(append(errs, err)...)
	}
	b.setDailyDone(today)
	if err := os.Remove(compressed); err != nil {
		errs = append(errs, fmt.Errorf("backup: remove snapshot: %v", err))
	}
	return errors.Join(errs...)
}

// snapshot writes the compressed snapshot of the store to compressed.
func (b *Backup) snapshot(ctx context.Context, compressed string) error {
	raw := strings.TrimSuffix(compressed, ".gz")
	if err := b.store.VacuumInto(ctx, raw); err != nil {
		return err
	}
	err := compress(raw, compressed)
	if removeErr := os.Remove(raw); err == nil && removeErr != nil {
		err = fmt.Errorf("backup: remove snapshot: %v", removeErr)
	}
	return err
}

func (b *Backup) setDailyDone(date string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dailyDone = date
}

func (b *Backup) put(ctx context.Context, key, path string, now time.Time) error {
	return putObject(ctx, b.cfg.Client, b.cfg.Endpoint, b.cfg.Region, b.cfg.Credentials, b.cfg.Prefix+"/"+key, path, now)
}

// dailyPending prefixes a kept snapshot whose daily upload is still to be done.
const dailyPending = "daily-"

// clearDir creates dir with mode 0700 if needed, removes what an earlier run left
// except the snapshots waiting for their daily upload, and returns those snapshots'
// names without the prefix, oldest first.
func clearDir(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("backup directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("backup directory: %v", err)
	}
	var pending []string
	for _, entry := range entries {
		if name, ok := strings.CutPrefix(entry.Name(), dailyPending); ok && entry.Type().IsRegular() && strings.HasSuffix(name, ".sqlite.gz") {
			pending = append(pending, name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return nil, fmt.Errorf("backup directory: %v", err)
		}
	}
	return pending, nil
}

// compress writes the gzip of the file at from to a new file at to.
func compress(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("backup: compress: %v", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: compress: %v", err)
	}
	writer := gzip.NewWriter(out)
	_, err = io.Copy(writer, in)
	if err := errors.Join(err, writer.Close(), out.Close()); err != nil {
		return fmt.Errorf("backup: compress: %v", err)
	}
	return nil
}
