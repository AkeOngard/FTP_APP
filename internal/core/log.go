package core

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// LogEntry is one line of the activity log shown in the UI.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // "info" | "error"
	Service string    `json:"service"`
	Message string    `json:"message"`
}

const logCapacity = 1000

// Logger keeps a bounded in-memory history and fans new entries out to subscribers.
type Logger struct {
	mu      sync.Mutex
	entries []LogEntry
	subs    map[int]func(LogEntry)
	nextSub int
}

func NewLogger() *Logger {
	return &Logger{subs: map[int]func(LogEntry){}}
}

func (l *Logger) Infof(service, format string, args ...any) {
	l.add("info", service, fmt.Sprintf(format, args...))
}

func (l *Logger) Errorf(service, format string, args ...any) {
	l.add("error", service, fmt.Sprintf(format, args...))
}

func (l *Logger) add(level, service, msg string) {
	e := LogEntry{Time: time.Now(), Level: level, Service: service, Message: cleanLogText(msg)}

	l.mu.Lock()
	if len(l.entries) >= logCapacity {
		l.entries = append(l.entries[:0], l.entries[len(l.entries)-logCapacity+1:]...)
	}
	l.entries = append(l.entries, e)
	subs := make([]func(LogEntry), 0, len(l.subs))
	for _, fn := range l.subs {
		subs = append(subs, fn)
	}
	l.mu.Unlock()

	// Called outside the lock so a slow subscriber cannot block the protocol handlers.
	for _, fn := range subs {
		fn(e)
	}
}

const maxLogMessage = 500

// cleanLogText makes text that partly comes from clients (user names, file names) safe to show as one
// log line: control characters, which could fake extra lines or hide text, become spaces, and very
// long messages are cut.
func cleanLogText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' || r == '‮' {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxLogMessage {
		cut := maxLogMessage
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// Entries returns a copy of the retained history, oldest first.
func (l *Logger) Entries() []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LogEntry{}, l.entries...)
}

// Subscribe registers fn for every new entry and returns a function that unregisters it.
func (l *Logger) Subscribe(fn func(LogEntry)) (cancel func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextSub
	l.nextSub++
	l.subs[id] = fn
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.subs, id)
	}
}
