package logx

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Logger struct {
	mu     sync.Mutex
	bufOut *bufio.Writer
}

func New(out io.Writer) *Logger {
	if out == nil {
		out = os.Stdout
	}
	return &Logger{bufOut: bufio.NewWriterSize(out, 1)}
}

func (l *Logger) Print(step string, msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] [%s] %s\n", ts, step, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.bufOut.WriteString(line)
	_ = l.bufOut.Flush()
}
