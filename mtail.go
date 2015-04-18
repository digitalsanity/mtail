// Copyright 2011 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/golang/glog"
	"github.com/google/mtail/exporter"
	"github.com/google/mtail/metrics"
	"github.com/google/mtail/tailer"
	"github.com/google/mtail/vm"

	_ "net/http/pprof"
)

var (
	port  = flag.String("port", "3903", "HTTP port to listen on.")
	logs  = flag.String("logs", "", "List of files to monitor.")
	progs = flag.String("progs", "", "Directory containing programs")

	oneShot = flag.Bool("one_shot", false, "Run once on a log file, dump json, and exit.")

	compileOnly = flag.Bool("compile_only", false, "Compile programs only, do not load the virtual machine.")

	dumpBytecode = flag.Bool("dump_bytecode", false, "Dump bytecode of programs and exit.")

	syslogUseCurrentYear = flag.Bool("syslog_use_current_year", true, "Patch yearless timestamps with the present year.")
)

type mtail struct {
	lines chan string   // Channel of lines from tailer to VM engine.
	store metrics.Store // Metrics storage.

	t *tailer.Tailer // t tails the watched files and feeds lines to the VMs.
	l *vm.Loader     // l loads programs and manages the VM lifecycle.

	webquit   chan struct{} // Channel to signal shutdown from web UI.
	closeOnce sync.Once     // Ensure shutdown happens only once.
}

func (m *mtail) OneShot(logfile string, lines chan string) error {
	defer m.Close()
	l, err := os.Open(logfile)
	if err != nil {
		return fmt.Errorf("failed to open log file %q: %s", logfile, err)
	}
	defer l.Close()

	r := bufio.NewReader(l)

	for {
		line, err := r.ReadString('\n')
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return fmt.Errorf("failed to read from %q: %s", logfile, err)
		default:
			lines <- line
		}
	}
}

func (m *mtail) StartTailing(pathnames []string) {
	o := tailer.Options{Lines: m.lines}
	m.t = tailer.New(o)
	if m.t == nil {
		glog.Fatal("Couldn't create a log tailer.")
	}

	for _, pathname := range pathnames {
		m.t.Tail(pathname)
	}
}

func (m *mtail) InitLoader(path string) {
	o := vm.LoaderOptions{Store: &m.store, Lines: m.lines, CompileOnly: *compileOnly, DumpBytecode: *dumpBytecode, SyslogUseCurrentYear: *syslogUseCurrentYear}
	m.l = vm.NewLoader(o)
	if m.l == nil {
		glog.Fatal("Couldn't create a program loader.")
	}
	errors := m.l.LoadProgs(path)
	if *compileOnly || *dumpBytecode {
		os.Exit(errors)
	}
}

func (m *mtail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte(`<a href="/json">json</a>, <a href="/metrics">prometheus metrics</a>`))
}

func newMtail() *mtail {
	return &mtail{
		lines:   make(chan string),
		webquit: make(chan struct{}),
	}
}

func (m *mtail) Serve() {
	if *progs == "" {
		glog.Fatalf("No mtail program directory specified; use -progs")
	}
	if *logs == "" {
		glog.Fatalf("No logs specified to tail; use -logs")
	}
	var pathnames []string
	for _, pathname := range strings.Split(*logs, ",") {
		if pathname != "" {
			pathnames = append(pathnames, pathname)
		}
	}
	if len(pathnames) == 0 {
		glog.Fatal("No logs to tail.")
	}

	m.InitLoader(*progs)

	ex := exporter.New(&m.store)

	if *oneShot {
		for _, pathname := range pathnames {
			err := m.OneShot(pathname, m.lines)
			if err != nil {
				glog.Fatalf("Failed one shot mode for %q: %s\n", pathname, err)
			}
		}
		b, err := json.MarshalIndent(m.store.Metrics, "", "  ")
		if err != nil {
			glog.Fatalf("Failed to marshal metrics into json: %s", err)
		}
		os.Stdout.Write(b)
		ex.WriteMetrics()
	} else {
		m.StartTailing(pathnames)

		http.Handle("/", m)
		http.HandleFunc("/json", http.HandlerFunc(ex.HandleJSON))
		http.HandleFunc("/metrics", http.HandlerFunc(ex.HandlePrometheusMetrics))
		http.HandleFunc("/quitquitquit", http.HandlerFunc(m.handleQuit))
		ex.StartMetricPush()

		go func() {
			err := http.ListenAndServe(":"+*port, nil)
			if err != nil {
				glog.Fatal(err)
			}
		}()
		m.shutdownHandler()
	}
}

func (m *mtail) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Add("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	fmt.Fprintf(w, "Exiting...")
	close(m.webquit)
}

// shutdownHandler handles external shutdown request events.
func (m *mtail) shutdownHandler() {
	n := make(chan os.Signal)
	signal.Notify(n, os.Interrupt, syscall.SIGTERM)
	select {
	case <-n:
		glog.Info("Received SIGTERM, exiting...")
	case <-m.webquit:
		glog.Info("Received Quit from UI, exiting...")
	}
	m.Close()
}

// Close handles the graceful shutdown of this mtail instance, ensuring that it only occurs once.
func (m *mtail) Close() {
	m.closeOnce.Do(func() {
		glog.Info("Shutdown requested.")
		if m.t != nil {
			m.t.Close()
		} else {
			glog.Info("Closing lines")
			close(m.lines)
		}
		if m.l != nil {
			<-m.l.VMsDone
		}
	})
}

func main() {
	flag.Parse()
	m := newMtail()
	m.Serve()
}
