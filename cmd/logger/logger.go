package logger

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ANSI color codes
const (
	Reset   = "\033[0m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	White   = "\033[37m"
	Gray    = "\033[90m"

	Bold = "\033[1m"
)

// Level represents log level
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String returns the string representation of level
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// Color returns the color for level
func (l Level) Color() string {
	switch l {
	case LevelDebug:
		return Gray
	case LevelInfo:
		return Green
	case LevelWarn:
		return Yellow
	case LevelError:
		return Red
	case LevelFatal:
		return Red + Bold
	default:
		return White
	}
}

// Logger is a colored logger
type Logger struct {
	mu       sync.Mutex
	level    Level
	output   io.Writer
	file     *os.File
	colored  bool
	quiet    bool
	verbose  bool
	prefix   string
}

// New creates a new logger
func New() *Logger {
	return &Logger{
		level:   LevelInfo,
		output:  os.Stdout,
		colored: true,
		quiet:   false,
		verbose: false,
	}
}

// SetLevel sets the minimum log level
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// SetQuiet sets quiet mode (only errors)
func (l *Logger) SetQuiet(quiet bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.quiet = quiet
	if quiet {
		l.level = LevelError
	}
}

// SetVerbose sets verbose mode (debug level)
func (l *Logger) SetVerbose(verbose bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbose = verbose
	if verbose {
		l.level = LevelDebug
	}
}

// SetColored enables/disables colored output
func (l *Logger) SetColored(colored bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.colored = colored
}

// SetPrefix sets log prefix
func (l *Logger) SetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prefix = prefix
}

// SetOutput sets the output writer
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.output = w
}

// SetLogFile sets output to a file (in addition to stdout)
func (l *Logger) SetLogFile(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	l.file = file
	return nil
}

// Close closes the log file if open
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// log writes a log message
func (l *Logger) log(level Level, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if level < l.level {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)

	var line string
	if l.colored {
		levelStr := fmt.Sprintf("%s%-5s%s", level.Color(), level.String(), Reset)
		if l.prefix != "" {
			line = fmt.Sprintf("%s%s%s %s [%s%s%s] %s\n", Gray, timestamp, Reset, levelStr, Cyan, l.prefix, Reset, msg)
		} else {
			line = fmt.Sprintf("%s%s%s %s %s\n", Gray, timestamp, Reset, levelStr, msg)
		}
	} else {
		if l.prefix != "" {
			line = fmt.Sprintf("%s %-5s [%s] %s\n", timestamp, level.String(), l.prefix, msg)
		} else {
			line = fmt.Sprintf("%s %-5s %s\n", timestamp, level.String(), msg)
		}
	}

	l.output.Write([]byte(line))

	// Also write to file without colors
	if l.file != nil {
		if l.prefix != "" {
			l.file.WriteString(fmt.Sprintf("%s %-5s [%s] %s\n", timestamp, level.String(), l.prefix, msg))
		} else {
			l.file.WriteString(fmt.Sprintf("%s %-5s %s\n", timestamp, level.String(), msg))
		}
	}
}

// Debug logs a debug message
func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LevelDebug, format, args...)
}

// Info logs an info message
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LevelInfo, format, args...)
}

// Warn logs a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LevelWarn, format, args...)
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LevelError, format, args...)
}

// Fatal logs a fatal message and exits
func (l *Logger) Fatal(format string, args ...interface{}) {
	l.log(LevelFatal, format, args...)
	os.Exit(1)
}

// Success logs a success message (info level with green color)
func (l *Logger) Success(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if LevelInfo < l.level {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)

	var line string
	if l.colored {
		line = fmt.Sprintf("%s%s%s %s✓%s %s\n", Gray, timestamp, Reset, Green, Reset, msg)
	} else {
		line = fmt.Sprintf("%s ✓ %s\n", timestamp, msg)
	}

	l.output.Write([]byte(line))

	if l.file != nil {
		l.file.WriteString(fmt.Sprintf("%s ✓ %s\n", timestamp, msg))
	}
}

// Status logs a status update (for tunnels, connections, etc.)
func (l *Logger) Status(icon, color, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if LevelInfo < l.level {
		return
	}

	msg := fmt.Sprintf(format, args...)

	var line string
	if l.colored {
		line = fmt.Sprintf("%s%s%s %s\n", color, icon, Reset, msg)
	} else {
		line = fmt.Sprintf("%s %s\n", icon, msg)
	}

	l.output.Write([]byte(line))
}

// Tunnel logs tunnel creation
func (l *Logger) Tunnel(name, url string) {
	l.Status("→", Cyan, "Tunnel %s%s%s available at %s%s%s", Bold, name, Reset+Cyan, Bold, url, Reset)
}

// Connected logs connection established
func (l *Logger) Connected(msg string) {
	l.Status("●", Green, "Connected: %s", msg)
}

// Disconnected logs connection lost
func (l *Logger) Disconnected(msg string) {
	l.Status("○", Yellow, "Disconnected: %s", msg)
}

// Reconnecting logs reconnection attempt
func (l *Logger) Reconnecting(attempt int, wait time.Duration) {
	l.Status("↻", Yellow, "Reconnecting (attempt %d) in %v...", attempt, wait)
}

// Request logs HTTP request (for request logging feature)
func (l *Logger) Request(method, path, clientIP string, status int, duration time.Duration, bytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if LevelInfo < l.level {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")

	var statusColor string
	switch {
	case status >= 500:
		statusColor = Red
	case status >= 400:
		statusColor = Yellow
	case status >= 300:
		statusColor = Cyan
	default:
		statusColor = Green
	}

	var line string
	if l.colored {
		line = fmt.Sprintf("%s%s%s %s%-4s%s %s%d%s %s %s%v%s %d bytes\n",
			Gray, timestamp, Reset,
			Blue, method, Reset,
			statusColor, status, Reset,
			path,
			Gray, duration, Reset,
			bytes)
	} else {
		line = fmt.Sprintf("%s %s %d %s %v %d bytes\n",
			timestamp, method, status, path, duration, bytes)
	}

	l.output.Write([]byte(line))

	if l.file != nil {
		l.file.WriteString(fmt.Sprintf("%s %s %s %d %s %v %d\n",
			timestamp, clientIP, method, status, path, duration, bytes))
	}
}

// Global default logger
var defaultLogger = New()

// Default returns the default logger
func Default() *Logger {
	return defaultLogger
}

// SetDefault sets the default logger
func SetDefault(l *Logger) {
	defaultLogger = l
}

// Package-level convenience functions
func Debug(format string, args ...interface{}) { defaultLogger.Debug(format, args...) }
func Info(format string, args ...interface{})  { defaultLogger.Info(format, args...) }
func Warn(format string, args ...interface{})  { defaultLogger.Warn(format, args...) }
func Error(format string, args ...interface{}) { defaultLogger.Error(format, args...) }
func Fatal(format string, args ...interface{}) { defaultLogger.Fatal(format, args...) }
func Success(format string, args ...interface{}) { defaultLogger.Success(format, args...) }
func Tunnel(name, url string) { defaultLogger.Tunnel(name, url) }
func Connected(msg string) { defaultLogger.Connected(msg) }
func Disconnected(msg string) { defaultLogger.Disconnected(msg) }
func Reconnecting(attempt int, wait time.Duration) { defaultLogger.Reconnecting(attempt, wait) }
