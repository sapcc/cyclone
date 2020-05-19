package pkg

import (
	"fmt"
	"io"
	llog "log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sapcc/go-bits/logg"
	"github.com/spf13/viper"
)

type logger struct {
	Prefix string
}

func (lg *logger) Printf(format string, args ...interface{}) {
	for _, v := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		l.Printf("[%s] %s", lg.Prefix, v)
	}
}

type compactLogger struct {
	sync.RWMutex
	lastMsg string
}

func (cl *compactLogger) Printf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	cl.RLock()
	if cl.lastMsg == msg {
		cl.RUnlock()
		return
	}
	cl.RUnlock()
	cl.Lock()
	defer cl.Unlock()
	cl.lastMsg = msg
	llog.Print(msg)
}

func (cl *compactLogger) Fatal(args ...interface{}) {
	llog.Fatal(args...)
}

var (
	l   *llog.Logger
	log compactLogger
)

func initLogger() {
	if l == nil {
		dir := filepath.Join(os.TempDir(), "cyclone")
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			log.Fatal(err)
		}
		fileName := time.Now().Format("20060102150405") + ".log"
		logFile, err := os.Create(filepath.Join(dir, fileName))
		if err != nil {
			log.Fatal(err)
		}

		symLink := filepath.Join(dir, "latest.log")
		if _, err := os.Lstat(symLink); err == nil {
			os.Remove(symLink)
		}

		err = os.Symlink(fileName, symLink)
		if err != nil {
			log.Printf("Failed to create a log symlink: %s", err)
		}

		// no need to close the log: https://golang.org/pkg/runtime/#SetFinalizer
		l = llog.New(logFile, llog.Prefix(), llog.Flags())

		logg.SetLogger(l)
		logg.ShowDebug = true

		if viper.GetBool("debug") {
			// write log into stderr and log file
			l.SetOutput(io.MultiWriter(llog.Writer(), l.Writer()))
		}

		// write stderr logs into the log file
		llog.SetOutput(io.MultiWriter(llog.Writer(), logFile))
	}
}
