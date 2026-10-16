package logging

import (
	"log"
	"os"
	"strings"
)

// A small leveled logger on top of the standard one, so the server's own
// lines (discovery, proxy health, requests) can be turned down or up from the
// config file.

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func ParseLevel(value string) Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

type Logger struct {
	level Level
	out   *log.Logger
}

func New(level string) *Logger {
	return &Logger{
		level: ParseLevel(level),
		out:   log.New(os.Stderr, "", log.LstdFlags),
	}
}

func (l *Logger) logf(level Level, name, format string, args ...any) {
	if l == nil || level < l.level {
		return
	}
	l.out.Printf(name+" "+format, args...)
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, "DEBUG", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, "INFO", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, "WARN", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, "ERROR", format, args...) }
