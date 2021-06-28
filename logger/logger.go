// Copyright (c) 2021 Arista Networks, Inc.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

// TODO: Implement ratelimiter

package logger

import (
	"errors"
	"fmt"
	"log"
	"log/syslog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	LogMgr *LogManager
)

const (
	EMERG int = iota
	ALERT
	CRIT
	ERR
	WARNING
	NOTICE
	INFO
	DEBUG
)

const (
	NoActionRequired = "No action is required -- this message is for information only."
)

func init() {
	LogMgr = NewLogManager()
}

func NewLogManager() *LogManager {
	sl, err := syslog.New(syslog.LOG_LOCAL4, filepath.Base(os.Args[0]))
	if err != nil {
		log.Fatal("Failed to create a syslog writer")
	}
	return &LogManager{
		writer:  sl,
		handles: make(map[LogID]*LogHandle),
	}
}

type LogManager struct {
	writer  *syslog.Writer
	handles map[LogID]*LogHandle
}

func (m *LogManager) Register(handle *LogHandle) error {
	if _, ok := m.handles[handle.id]; ok {
		return errors.New(fmt.Sprintf("Log handle for ID %s is alreaduy registered", handle.id.String()))
	}
	m.handles[handle.id] = handle
	return nil
}

func (m *LogManager) Fire(id LogID, message string) error {
	message = fmt.Sprintf("%%%s-%d-%s: %s", id.Facility, m.handles[id].severity, id.Mnenmonic, message)
	switch m.handles[id].severity {
	case ALERT:
		m.writer.Alert(message)
	case CRIT:
		m.writer.Crit(message)
	case ERR:
		m.writer.Err(message)
	case WARNING:
		m.writer.Warning(message)
	case NOTICE:
		m.writer.Notice(message)
	case INFO:
		m.writer.Info(message)
	case DEBUG:
		m.writer.Debug(message)
	default:
		return errors.New(fmt.Sprintf("invalid serverity %d", m.handles[id].severity))
	}
	return nil
}

func NewLogID(id string) (*LogID, error) {
	parts := strings.SplitN(id, "_", 2)

	if len(parts) != 2 {
		return nil, errors.New("Failed split log ID")
	}

	return &LogID{
		Facility:  parts[0],
		Mnenmonic: parts[1],
	}, nil
}

type LogID struct {
	Facility  string
	Mnenmonic string
}

func (l *LogID) String() string {
	return fmt.Sprintf("%s_%s", l.Facility, l.Mnenmonic)
}

type LogHandle struct {
	id                LogID
	severity          int
	format            string
	explanation       string
	recommendedAction string
	minInterval       time.Duration
	rateLimitArgs     int
}

func (h *LogHandle) Log(args ...interface{}) {
	err := LogMgr.Fire(h.id, fmt.Sprintf(h.format, args...))
	if err != nil {
		log.Printf("Failed to log message %s", h.id.String())
	}
}

func NewLogHandle(
	id string,
	serverity int,
	format string,
	explanation string,
	recommendedAction string,
	minInterval time.Duration,
	rateLimitArgs int) func(args ...interface{}) {

	logID, err := NewLogID(id)
	if err != nil {
		log.Fatal("Failed to determine log id")
	}

	lh := &LogHandle{
		id:                *logID,
		severity:          serverity,
		format:            format,
		explanation:       explanation,
		recommendedAction: recommendedAction,
		minInterval:       minInterval,
		rateLimitArgs:     rateLimitArgs,
	}
	err = LogMgr.Register(lh)
	if err != nil {
		log.Fatalf("Log ID %s is already registered", logID.String())
	}
	return lh.Log
}

var LogD = NewLogHandle
