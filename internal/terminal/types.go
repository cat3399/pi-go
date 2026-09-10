// Package terminal owns session-scoped interactive PTYs. A terminal is a byte
// stream, not a sequence of shell-wrapped commands. Readers have independent
// cursors; neither a reader nor a surface attachment owns the process.
package terminal

import (
	"errors"
	"time"
)

const (
	MaxWait        = 30 * time.Second
	DefaultWait    = time.Second
	MaxInputBytes  = 64 * 1024
	MaxOutputBytes = 50 * 1024
	MaxOutputLines = 2000
	bufferBytes    = 1024 * 1024
	logBytes       = 32 * 1024 * 1024
	maxTerminals   = 16
	maxHistory     = 32
	maxSpans       = 4096
	writeTimeout   = 2 * time.Second
	settleTimeout  = 2 * time.Second
)

var (
	ErrClosed   = errors.New("terminal service is closed")
	ErrNotFound = errors.New("terminal not found in this session")
	ErrExited   = errors.New("terminal has exited")
)

type Actor string

const (
	Agent Actor = "agent"
	User  Actor = "user"
)

type Action string

const (
	Open      Action = "open"
	Run       Action = "run"
	Read      Action = "read"
	Write     Action = "write"
	Resize    Action = "resize"
	Interrupt Action = "interrupt"
	Close     Action = "close"
	List      Action = "list"
)

// Request is shared by the tool and typed Application API. Wait bounds only
// this operation, never the lifetime of the shell. Interrupt/Close are explicit.
type Request struct {
	Action      Action
	ID          string
	Command     string
	Input       string
	CWD         string
	Title       string
	After       *int64
	Mode        string // since (default), tail, follow
	Wait        *time.Duration
	MaxBytes    int
	MaxLines    int
	IncludeUser bool
	Cols        int
	Rows        int
}

type Info struct {
	ID        string
	Title     string
	CWD       string // initial directory; the shell owns subsequent directory changes
	Shell     string
	State     string // running or exited; says nothing about command completion
	ExitCode  *int
	Error     string
	LogPath   string
	CreatedAt time.Time
	Cols      int
	Rows      int
}

type Result struct {
	Terminal  *Info
	Terminals []Info
	Data      []byte // raw PTY bytes for terminal emulators
	Text      string // bounded text projection for model/tool consumers
	Start     int64
	Cursor    int64
	Truncated bool
	HasMore   bool
	Reason    string // data, idle, deadline, limit, exited
}

type Options struct {
	CWD         string
	Shell       string
	Environment []string
	// Resolve is evaluated only when creating a terminal. Live settings changes
	// apply to new shells; existing shells keep their environment and state.
	Resolve func() (Defaults, error)
}

type Defaults struct {
	CWD         string
	Shell       string
	Environment []string
}
