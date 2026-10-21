package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// kagent's UI renders tool activity from DataParts tagged with kagent_type.
const (
	functionCall     = "function_call"
	functionResponse = "function_response"
)

// droidEvent is one line of `droid exec -o stream-json`.
type droidEvent struct {
	Type       string         `json:"type"`
	Subtype    string         `json:"subtype"`
	Role       string         `json:"role"`
	Text       string         `json:"text"`
	SessionID  string         `json:"session_id"`
	ID         string         `json:"id"`
	ToolName   string         `json:"toolName"`
	Parameters map[string]any `json:"parameters"`
	Value      any            `json:"value"`
	IsError    bool           `json:"isError"`
	FinalText  string         `json:"finalText"`
	Message    string         `json:"message"`
}

// droidExecutor runs one `droid exec` process per A2A turn. A kagent Session
// (A2A context) maps to one droid session ID, continued with `-s` on the next
// turn. No droid process outlives its turn, so the Actor snapshot Substrate
// takes between turns holds only this Go process, which restores cleanly.
type droidExecutor struct {
	cfg *harnessConfig
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]string // A2A context ID -> droid session ID, persisted in stateFile
	locks    map[string]*sync.Mutex
	running  map[a2a.TaskID]*exec.Cmd
}

func newDroidExecutor(cfg *harnessConfig, log *slog.Logger) *droidExecutor {
	e := &droidExecutor{cfg: cfg, log: log, sessions: map[string]string{}, locks: map[string]*sync.Mutex{}, running: map[a2a.TaskID]*exec.Cmd{}}
	if b, err := os.ReadFile(cfg.StateFile); err == nil {
		json.Unmarshal(b, &e.sessions)
	}
	return e
}

// sessionFor reads the map from disk each time: process memory was not carried
// from one Substrate turn to the next, but the Actor's /data volume is.
func (e *droidExecutor) sessionFor(contextID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b, err := os.ReadFile(e.cfg.StateFile); err == nil {
		json.Unmarshal(b, &e.sessions)
	}
	return e.sessions[contextID]
}

func (e *droidExecutor) saveSession(contextID, sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions[contextID] = sessionID
	b, _ := json.Marshal(e.sessions)
	tmp := e.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, e.cfg.StateFile)
	} else {
		e.log.Warn("could not persist droid session map", "error", err)
	}
}

func (e *droidExecutor) contextLock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[id]
	if !ok {
		l = &sync.Mutex{}
		e.locks[id] = l
	}
	return l
}

func (e *droidExecutor) args(sessionID, prompt string) []string {
	args := []string{"exec", "-o", "stream-json", "--cwd", e.cfg.Cwd}
	if e.cfg.Autonomy != "" && e.cfg.Autonomy != "off" {
		args = append(args, "--auto", e.cfg.Autonomy)
	}
	if e.cfg.Model != "" {
		args = append(args, "-m", e.cfg.Model)
	}
	if e.cfg.Agent.Instruction != "" {
		args = append(args, "--append-system-prompt", e.cfg.Agent.Instruction)
	}
	if sessionID != "" {
		args = append(args, "-s", sessionID)
	}
	return append(args, prompt)
}

func dataPart(data map[string]any, kagentType string) *a2a.Part {
	p := a2a.NewDataPart(data)
	p.Metadata = map[string]any{"kagent_type": kagentType}
	return p
}

func (e *droidExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		status := func(state a2a.TaskState, parts ...*a2a.Part) bool {
			var msg *a2a.Message
			if len(parts) > 0 {
				msg = a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)
			}
			return yield(a2a.NewStatusUpdateEvent(ec, state, msg), nil)
		}
		fail := func(text string) { status(a2a.TaskStateFailed, a2a.NewTextPart(text)) }

		var prompt []string
		for _, p := range ec.Message.Parts {
			if t := p.Text(); t != "" {
				prompt = append(prompt, t)
			}
		}
		if len(prompt) == 0 {
			fail("empty prompt")
			return
		}
		if !status(a2a.TaskStateWorking) {
			return
		}

		lock := e.contextLock(ec.ContextID)
		lock.Lock()
		defer lock.Unlock()

		sessionID := e.sessionFor(ec.ContextID)

		cmd := exec.CommandContext(ctx, "droid", e.args(sessionID, strings.Join(prompt, "\n"))...)
		cmd.Dir = e.cfg.Cwd
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fail(err.Error())
			return
		}
		if err := cmd.Start(); err != nil {
			fail("start droid: " + err.Error())
			return
		}
		e.mu.Lock()
		e.running[ec.TaskID] = cmd
		e.mu.Unlock()
		defer func() {
			// Reap droid and anything it spawned, so nothing but this process
			// is left in the Actor when Substrate checkpoints it.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			e.mu.Lock()
			delete(e.running, ec.TaskID)
			e.mu.Unlock()
		}()
		e.log.Info("droid turn started", "context", ec.ContextID, "droid_session", sessionID, "model", e.cfg.Model)

		var final, errText string
		completed := false
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for scan.Scan() {
			var ev droidEvent
			if json.Unmarshal(scan.Bytes(), &ev) != nil {
				continue
			}
			if ev.SessionID != "" && sessionID == "" {
				sessionID = ev.SessionID
				e.saveSession(ec.ContextID, sessionID)
			}
			var part *a2a.Part
			switch ev.Type {
			case "tool_call":
				part = dataPart(map[string]any{"id": ev.ID, "name": ev.ToolName, "args": ev.Parameters}, functionCall)
			case "tool_result":
				part = dataPart(map[string]any{"id": ev.ID, "name": ev.ToolName, "response": map[string]any{"result": ev.Value, "isError": ev.IsError}}, functionResponse)
			case "message":
				if ev.Role == "assistant" && ev.Text != "" {
					part = a2a.NewTextPart(ev.Text)
				}
			case "completion":
				final, completed = ev.FinalText, true
			case "error":
				errText = ev.Message
				e.log.Warn("droid error", "message", ev.Message)
			}
			if part != nil && !status(a2a.TaskStateWorking, part) {
				return
			}
		}
		waitErr := cmd.Wait()

		switch {
		case ctx.Err() != nil:
			status(a2a.TaskStateCanceled)
		case completed:
			if yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(final)), nil) {
				status(a2a.TaskStateCompleted)
			}
		default:
			if errText == "" {
				errText = fmt.Sprintf("droid exited (%v): %s", waitErr, tail(stderr.String(), 600))
			}
			fail(errText)
		}
		e.log.Info("droid turn ended", "context", ec.ContextID, "droid_session", sessionID, "completed", completed)
	}
}

func (e *droidExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.mu.Lock()
		cmd := e.running[ec.TaskID]
		e.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
