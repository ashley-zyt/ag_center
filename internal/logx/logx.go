package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu         sync.Mutex
	fileWriter *DailyFileWriter
	termCh     chan string
	done       chan struct{}
	closed     bool
}

// DailyFileWriter 按日期自动切分日志文件
type DailyFileWriter struct {
	mu        sync.Mutex
	dir       string
	file      *os.File
	currentDay string
}

func NewDailyFileWriter(dir string) (*DailyFileWriter, error) {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return nil, err
	}
	w := &DailyFileWriter{dir: dir}
	if err := w.ensureFile(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *DailyFileWriter) ensureFile() error {
	today := time.Now().Format("20060102")
	if w.file != nil && w.currentDay == today {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	path := filepath.Join(w.dir, today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.currentDay = today
	return nil
}

func (w *DailyFileWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureFile(); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *DailyFileWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Sync()
	}
	return nil
}

func (w *DailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// New 创建一个新的Logger，同时写入文件和终端（终端异步写入避免阻塞）
func New(out io.Writer) *Logger {
	if out == nil {
		out = os.Stdout
	}
	l := &Logger{
		termCh: make(chan string, 1000),
		done:   make(chan struct{}),
	}

	// 尝试创建日志文件写入器
	logDir := "logs"
	fw, err := NewDailyFileWriter(logDir)
	if err != nil {
		// 文件写入失败，降级为仅终端输出
		l.mu.Lock()
		l.fileWriter = nil
		l.mu.Unlock()
		// 启动异步终端写入
		go l.termWriteLoop(out)
		l.Print("LOGX", fmt.Sprintf("日志系统初始化完成 writeFile=false writeTerm=true (原因: %v)", err))
	} else {
		l.mu.Lock()
		l.fileWriter = fw
		l.mu.Unlock()
		// 启动异步终端写入
		go l.termWriteLoop(out)
		l.Print("LOGX", fmt.Sprintf("日志系统初始化完成 writeFile=true writeTerm=true"))
		l.Print("LOGX", fmt.Sprintf("日志文件路径: %s/", logDir))
	}

	return l
}

func (l *Logger) termWriteLoop(out io.Writer) {
	for {
		select {
		case msg, ok := <-l.termCh:
			if !ok {
				return
			}
			_, _ = io.WriteString(out, msg)
			if f, ok := out.(*os.File); ok {
				_ = f.Sync()
			}
		case <-l.done:
			// 排空剩余消息
			for {
				select {
				case msg := <-l.termCh:
					_, _ = io.WriteString(out, msg)
				default:
					return
				}
			}
		}
	}
}

func (l *Logger) Print(step string, msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] [%s] %s\n", ts, step, msg)

	// 同步写文件（避免日志丢失）
	l.mu.Lock()
	fw := l.fileWriter
	l.mu.Unlock()
	if fw != nil {
		_, _ = fw.Write([]byte(line))
		_ = fw.Sync()
	}

	// 异步写终端（避免终端Quick Edit模式阻塞业务协程）
	if !l.closed {
		select {
		case l.termCh <- line:
		default:
			// channel满时丢弃终端日志，避免阻塞
		}
	}
}

// Close 刷新剩余日志并关闭文件
func (l *Logger) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	fw := l.fileWriter
	l.fileWriter = nil
	l.mu.Unlock()

	close(l.done)

	if fw != nil {
		_ = fw.Close()
	}
}

func ForceFlushStdout() {
	_ = os.Stdout.Sync()
}

func ForceFlushStderr() {
	_ = os.Stderr.Sync()
}
