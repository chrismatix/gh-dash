// Package prypane runs `gh pry` for the selected PR in a pseudo-terminal and
// renders its screen in place of the built-in PR preview.
package prypane

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Moving through the list with j/k would otherwise spawn a bun process per row.
const startDelay = 150 * time.Millisecond

type (
	startMsg  struct{ generation int }
	outputMsg struct{}
	exitedMsg struct{ generation int }
	diffMsg   struct {
		generation int
		command    string
	}
)

type launch struct {
	repoPath string
	repoName string
	number   int
}

type Model struct {
	Focused bool

	events     chan tea.Msg
	generation int
	target     launch
	running    bool
	width      int
	height     int
	emulator   *vt.SafeEmulator
	terminal   *os.File
	process    *exec.Cmd
}

func NewModel() Model {
	return Model{events: make(chan tea.Msg, 1)}
}

// Listen must be running at all times; Update re-arms it after every pry message.
func (m *Model) Listen() tea.Cmd {
	events := m.events
	return func() tea.Msg { return <-events }
}

// Update reports whether msg belonged to the pane.
func (m *Model) Update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case startMsg:
		if msg.generation == m.generation {
			m.start()
		}
	case exitedMsg:
		if msg.generation == m.generation {
			m.running = false
			m.Focused = false
			_, _ = m.emulator.Write([]byte("\r\n\x1b[2m[gh pry exited, enter to reopen]\x1b[m"))
		}
	case diffMsg:
		if msg.generation == m.generation {
			diff := exec.Command("sh", "-c", msg.command)
			diff.Dir = m.process.Dir
			return tea.Batch(m.Listen(), tea.ExecProcess(diff, nil)), true
		}
	case outputMsg:
	default:
		return nil, false
	}
	return m.Listen(), true
}

// Open shows the given PR, restarting gh pry only when the PR changed.
func (m *Model) Open(repoPath, repoName string, number, width, height int) {
	m.resize(width, height)
	target := launch{repoPath: repoPath, repoName: repoName, number: number}
	if target == m.target {
		return
	}
	m.stop()
	m.target = target
	m.emulator = vt.NewSafeEmulator(max(width, 1), max(height, 1))
	m.generation++
	generation := m.generation
	time.AfterFunc(startDelay, func() { m.events <- startMsg{generation} })
}

func (m *Model) Focus() {
	if !m.running && m.target.number != 0 {
		m.stop()
		m.generation++
		m.emulator = vt.NewSafeEmulator(max(m.width, 1), max(m.height, 1))
		m.start()
	}
	m.Focused = m.running
}

// Blur hands esc to gh pry as well, so leaving never strands it in a reply box.
func (m *Model) Blur(esc tea.KeyPressMsg) {
	m.SendKey(esc)
	m.Focused = false
}

func (m *Model) SendKey(msg tea.KeyPressMsg) {
	if m.running {
		m.emulator.SendKey(uv.KeyPressEvent(uv.Key(tea.Key(msg))))
	}
}

func (m *Model) Paste(text string) {
	if m.running {
		m.emulator.Paste(text)
	}
}

func (m *Model) View() string {
	if m.emulator == nil {
		return ""
	}
	return m.emulator.Render()
}

func (m *Model) start() {
	process := exec.Command("gh", "pry", "-R", m.target.repoName, strconv.Itoa(m.target.number))
	process.Dir = m.target.repoPath
	if _, err := os.Stat(process.Dir); process.Dir == "" || err != nil {
		process.Dir, _ = os.UserHomeDir()
	}
	// gh pry writes its diff command to fd 3 instead of opening hunk inside the pane.
	diffReader, diffWriter, err := os.Pipe()
	if err != nil {
		_, _ = m.emulator.Write([]byte(fmt.Sprintf("could not start gh pry: %v", err)))
		return
	}
	process.ExtraFiles = []*os.File{diffWriter}
	process.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "GH_PRY_DIFF_FD=3")

	terminal, err := pty.StartWithSize(process, &pty.Winsize{Rows: uint16(m.height), Cols: uint16(m.width)})
	_ = diffWriter.Close()
	if err != nil {
		_ = diffReader.Close()
		_, _ = m.emulator.Write([]byte(fmt.Sprintf("could not start gh pry: %v", err)))
		return
	}
	m.process, m.terminal, m.running = process, terminal, true

	emulator, events, generation := m.emulator, m.events, m.generation
	go func() {
		defer diffReader.Close()
		lines := bufio.NewScanner(diffReader)
		for lines.Scan() {
			events <- diffMsg{generation, lines.Text()}
		}
	}()
	go func() { _, _ = io.Copy(terminal, emulator) }()
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				_, _ = emulator.Write(buffer[:n])
				select {
				case events <- outputMsg{}:
				default:
				}
			}
			if err != nil {
				break
			}
		}
		_ = process.Wait()
		events <- exitedMsg{generation}
	}()
}

func (m *Model) stop() {
	if m.running {
		// pty.Start puts gh pry in its own session, so this reaches bun and its children too.
		_ = syscall.Kill(-m.process.Process.Pid, syscall.SIGTERM)
		_ = m.terminal.Close()
		m.running = false
	}
	if m.emulator != nil {
		_ = m.emulator.Close()
	}
	m.Focused = false
}

func (m *Model) resize(width, height int) {
	if width == m.width && height == m.height {
		return
	}
	m.width, m.height = width, height
	if m.emulator != nil {
		m.emulator.Resize(max(width, 1), max(height, 1))
	}
	if m.running {
		_ = pty.Setsize(m.terminal, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)})
	}
}
