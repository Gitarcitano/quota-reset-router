package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const maxReservesFileSize = 1 << 20

// reservesDocument is the shape of the accounts file.
type reservesDocument struct {
	Accounts map[string]accountReserve `yaml:"accounts"`
}

// reservesFile keeps the per-account reserves read from accounts_file. The file is shared
// between machines by a sync tool, so it is re-read whenever its size or modification time
// changes. A missing or invalid file keeps the last good reserves.
type reservesFile struct {
	path     string
	mu       sync.Mutex // serializes reads and writes of the file
	modTime  time.Time
	size     int64
	loaded   atomic.Pointer[map[string]accountReserve]
	loadedAt atomic.Pointer[time.Time]
	err      atomic.Pointer[string]
}

func newReservesFile(path string) *reservesFile {
	return &reservesFile{path: path}
}

// accounts returns the last good reserves and whether the file was ever read successfully.
func (f *reservesFile) accounts() (map[string]accountReserve, bool) {
	if loaded := f.loaded.Load(); loaded != nil {
		return *loaded, true
	}
	return nil, false
}

func (f *reservesFile) pathValue() string {
	return f.path
}

func (f *reservesFile) lastError() string {
	if err := f.err.Load(); err != nil {
		return *err
	}
	return ""
}

func (f *reservesFile) lastLoad() *time.Time {
	return f.loadedAt.Load()
}

func (f *reservesFile) setError(err error) {
	if err == nil {
		f.err.Store(nil)
		return
	}
	message := err.Error()
	f.err.Store(&message)
}

// refresh re-reads the file when it changed since the last read. It reports whether new
// reserves were loaded.
func (f *reservesFile) refresh() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshLocked(false)
}

func (f *reservesFile) refreshLocked(force bool) bool {
	info, err := os.Stat(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			f.setError(errors.New("accounts_file not found"))
		} else {
			f.setError(errors.New("accounts_file cannot be read"))
		}
		return false
	}
	if !force && f.loaded.Load() != nil && info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return false
	}
	if info.Size() > maxReservesFileSize {
		f.setError(errors.New("accounts_file is larger than 1 MiB"))
		return false
	}
	file, err := os.Open(f.path)
	if err != nil {
		f.setError(errors.New("accounts_file cannot be read"))
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxReservesFileSize+1))
	file.Close()
	if err != nil {
		f.setError(errors.New("accounts_file cannot be read"))
		return false
	}
	accounts, err := parseReserves(raw)
	if err != nil {
		// Remember the bad version so it is not parsed again every pass; a later edit retries.
		f.modTime, f.size = info.ModTime(), info.Size()
		f.setError(err)
		return false
	}
	f.modTime, f.size = info.ModTime(), info.Size()
	now := time.Now()
	f.loaded.Store(&accounts)
	f.loadedAt.Store(&now)
	f.setError(nil)
	return true
}

func parseReserves(raw []byte) (map[string]accountReserve, error) {
	var doc reservesDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("accounts_file is not valid YAML")
	}
	if err := validateAccounts(doc.Accounts); err != nil {
		return nil, fmt.Errorf("accounts_file: %w", err)
	}
	if doc.Accounts == nil {
		doc.Accounts = map[string]accountReserve{}
	}
	return doc.Accounts, nil
}

// write replaces the file atomically (temp file in the same directory, then rename) so a sync
// tool or a concurrent reader never sees a partial file, then loads it.
func (f *reservesFile) write(accounts map[string]accountReserve) error {
	if err := validateAccounts(accounts); err != nil {
		return err
	}
	if accounts == nil {
		accounts = map[string]accountReserve{}
	}
	body, err := yaml.Marshal(reservesDocument{Accounts: accounts})
	if err != nil {
		return errors.New("accounts could not be encoded")
	}
	raw := append([]byte("# Managed by quota-reset-router. Shared between machines; edits on either side are applied on both.\n"), body...)
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, base := filepath.Split(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("accounts_file directory cannot be created")
	}
	// The dot prefix and .tmp suffix let sync tools ignore the temp file.
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return errors.New("accounts_file cannot be written")
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return errors.New("accounts_file cannot be written")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("accounts_file cannot be written")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("accounts_file cannot be written")
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return errors.New("accounts_file cannot be written")
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		return errors.New("accounts_file cannot be written")
	}
	if !f.refreshLocked(true) {
		if message := f.lastError(); message != "" {
			return errors.New(message)
		}
	}
	return nil
}
