package sessionlog

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Session struct {
	path string
	file *os.File
}

const staleLogAge = 7 * 24 * time.Hour

func Start() (*Session, error) {
	dir, err := logDir()
	if err != nil {
		return nil, err
	}
	cleanupStaleLogs(dir)

	name := time.Now().Format("20060102-150405") + ".log"
	path := filepath.Join(dir, name)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stderr, file))

	return &Session{
		path: path,
		file: file,
	}, nil
}

func (s *Session) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Session) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	log.SetOutput(os.Stderr)
	return s.file.Close()
}

func Dir() (string, error) {
	return logDir()
}

func logDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}

	dir := filepath.Join(base, "vocis", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func Infof(format string, args ...any) {
	log.Printf("INFO  "+format, args...)
}

func Warnf(format string, args ...any) {
	log.Printf("WARN  "+format, args...)
}

func Errorf(format string, args ...any) {
	log.Printf("ERROR "+format, args...)
}

func cleanupStaleLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleLogAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
