// Copyright (c) 2020 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/common-go/process"
	"github.com/gitpod-io/gitpod/supervisor/pkg/supervisor"
	"github.com/prometheus/procfs"
	reaper "github.com/ramr/go-reaper"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "init the supervisor",
	Run: func(cmd *cobra.Command, args []string) {
		log.Init(ServiceName, Version, true, false)
		cfg, err := supervisor.GetConfig()
		if err != nil {
			log.WithError(err).Info("cannnot load config")
		}
		var (
			sigInput = make(chan os.Signal, 1)
		)
		signal.Notify(sigInput, os.Interrupt, syscall.SIGTERM)

		supervisorPath, err := os.Executable()
		if err != nil {
			supervisorPath = "/.supervisor/supervisor"
		}
		runCommand := exec.Command(supervisorPath, "run")
		runCommand.Args[0] = "supervisor"
		runCommand.Stdin = os.Stdin
		runCommand.Stdout = os.Stdout
		runCommand.Stderr = os.Stderr
		runCommand.Env = os.Environ()
		err = runCommand.Start()
		if err != nil {
			log.WithError(err).Error("supervisor run start error")
			return
		}

		supervisorDone := make(chan struct{})
		go func() {
			defer close(supervisorDone)

			err := runCommand.Wait()
			if err != nil && !(strings.Contains(err.Error(), "signal: interrupt") || strings.Contains(err.Error(), "no child processes")) {
				log.WithError(err).Error("supervisor run error")
				return
			}
		}()
		// start the reaper to clean up zombie processes
		reaper.Reap()

		select {
		case <-supervisorDone:
			// supervisor has ended - we're all done here
			return
		case <-sigInput:
			// we received a terminating signal - pass on to supervisor and wait for it to finish
			ctx, cancel := context.WithTimeout(context.Background(), cfg.GetTerminationGracePeriod())
			defer cancel()
			slog := newShutdownLogger()
			defer slog.Close()
			slog.write("Shutting down all processes")

			terminationDone := make(chan struct{})
			go func() {
				defer close(terminationDone)
				slog.TerminateSync(ctx, runCommand.Process.Pid)
				terminateAllProcesses(ctx, slog)
				close(supervisorDone)
			}()
			// wait for either successful termination or the timeout
			select {
			case <-ctx.Done():
				// Time is up, but we give all the goroutines a bit more time to react to this.
				time.Sleep(time.Millisecond * 500)
			case <-terminationDone:
			}
			slog.write("Finished shutting down all processes.")
		}
	},
}

// terminateAllProcesses terminates all processes but ours until there are none anymore or the context is cancelled
// on context cancellation any still running processes receive a SIGKILL
func terminateAllProcesses(ctx context.Context, slog shutdownLogger) {
	for {
		processes, err := procfs.AllProcs()
		if err != nil {
			log.WithError(err).Error("Cannot list processes")
			slog.write(fmt.Sprintf("Cannot list processes: %s", err))
			return
		}
		// only one process (must be us)
		if len(processes) == 1 {
			return
		}
		// terminate all processes but ourself
		var wg sync.WaitGroup
		for _, proc := range processes {
			if proc.PID == os.Getpid() {
				continue
			}
			p := proc
			wg.Add(1)
			go func() {
				defer wg.Done()
				slog.TerminateSync(ctx, p.PID)
			}()
		}
		wg.Wait()
	}
}

func init() {
	rootCmd.AddCommand(initCmd)
}

type shutdownLogger interface {
	write(s string)
	TerminateSync(ctx context.Context, pid int)
	io.Closer
}

func newShutdownLogger() shutdownLogger {
	file := "/workspace/.gitpod/supervisor-termination.log"
	f, err := os.Create(file)
	if err != nil {
		log.WithError(err).WithField("file", file).Error("Couldn't create shutdown log file")
	}
	result := shutdownLoggerImpl{
		file:      f,
		startTime: time.Now(),
	}
	return &result
}

type shutdownLoggerImpl struct {
	file      *os.File
	startTime time.Time
}

func (l *shutdownLoggerImpl) write(s string) {
	if l.file != nil {
		_, err := l.file.WriteString(fmt.Sprintf("[%s] %s \n", time.Since(l.startTime), s))
		if err != nil {
			log.WithError(err).Error("couldn't write to log file")
		}
	} else {
		log.Debug(s)
	}
}
func (l *shutdownLoggerImpl) Close() error {
	return l.file.Close()
}
func (l *shutdownLoggerImpl) TerminateSync(ctx context.Context, pid int) {
	proc, err := procfs.NewProc(pid)
	if err != nil {
		l.write(fmt.Sprintf("Couldn't obtain process information for PID %d.", pid))
		return
	}
	stat, err := proc.Stat()
	if err != nil {
		l.write(fmt.Sprintf("Couldn't obtain process information for PID %d.", pid))
	} else if stat.State == "Z" {
		return
	} else {
		l.write(fmt.Sprintf("Terminating process %s with PID %d (state: %s, cmdlind: %s).", stat.Comm, pid, stat.State, fmt.Sprint(proc.CmdLine())))
	}
	err = process.TerminateSync(ctx, pid)
	if err != nil {
		if err == process.ErrForceKilled {
			l.write("Terminating process didn't finish, but had to be force killed")
		} else {
			l.write(fmt.Sprintf("Terminating main process errored: %s", err))
		}
	}
}
