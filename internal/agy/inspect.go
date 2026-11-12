package agy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// procfs facts (Linux): /proc/<pid>/stat fields after the parenthesized
// command are state, ppid, pgrp, ..., utime (12th), stime (13th) in clock
// ticks, which /proc reports at USER_HZ = 100. agy's workers stay in the
// front process's group, which StartProc makes the front's own pid.
const clockTicksPerSecond = 100

// Resources is a process's memory and CPU use.
type Resources struct {
	RSSBytes   int64
	CPUSeconds float64
}

// Worker is one --process-per-session worker of a front process.
type Worker struct {
	Number int
	PID    int
	// Sessions are the sessions the front last routed to this worker.
	Sessions  []string
	Resources Resources
}

// ProcInfo describes a running agy process.
type ProcInfo struct {
	PID       int
	Agent     string
	Started   time.Time
	Resources Resources
	// Workers are the front's live workers.
	Workers []Worker
}

var servedBy = regexp.MustCompile(`session (\S+) served by worker (\d+)`)

// Inspect reports the process, its live workers, and their resources.
func (p *Proc) Inspect() (ProcInfo, error) {
	pid := p.cmd.Process.Pid
	info := ProcInfo{PID: pid, Agent: p.cfg.Agent, Started: p.started}
	res, _, err := readResources(pid)
	if err != nil {
		return info, err
	}
	info.Resources = res
	front, err := p.readLog("acp-front", pid)
	if err != nil {
		return info, nil // no worker started yet
	}
	info.Workers = liveWorkers(front, pid)
	return info, nil
}

// liveWorkers lists the workers the front log started that still run in
// the front's process group, with the sessions last routed to each.
func liveWorkers(front string, frontPID int) []Worker {
	sessionsOf := map[int][]string{}
	latest := map[string]int{}
	var order []string
	for _, m := range servedBy.FindAllStringSubmatch(front, -1) {
		n, _ := strconv.Atoi(m[2])
		if _, seen := latest[m[1]]; !seen {
			order = append(order, m[1])
		}
		latest[m[1]] = n
	}
	for _, session := range order {
		sessionsOf[latest[session]] = append(sessionsOf[latest[session]], session)
	}

	var workers []Worker
	for _, m := range startedWorker.FindAllStringSubmatch(front, -1) {
		n, _ := strconv.Atoi(m[1])
		pid, _ := strconv.Atoi(m[2])
		res, group, err := readResources(pid)
		if err != nil || group != frontPID {
			continue // exited, or the pid now belongs to someone else
		}
		workers = append(workers, Worker{Number: n, PID: pid, Sessions: sessionsOf[n], Resources: res})
	}
	return workers
}

// readResources reads a process's resources and process group.
func readResources(pid int) (Resources, int, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Resources{}, 0, err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return Resources{}, 0, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 13 {
		return Resources{}, 0, errors.New("short /proc stat")
	}
	group, _ := strconv.Atoi(fields[2])
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	res := Resources{CPUSeconds: float64(utime+stime) / clockTicksPerSecond}

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return Resources{}, 0, err
	}
	for line := range strings.Lines(string(status)) {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			res.RSSBytes = kb << 10
		}
	}
	return res, group, nil
}

// CloseSession ends the session in this process, and its worker exits.
// The conversation stays on disk, so ReopenSession can bring it back.
func (p *Proc) CloseSession(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	if err := p.call(ctx, "session/close", map[string]string{"sessionId": sessionID}, nil); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.live, sessionID)
	p.mu.Unlock()
	return nil
}
