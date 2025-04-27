package main

import (
	"context"
	"fmt"
	"github.com/charmbracelet/bubbles/v2/spinner"
	"github.com/charmbracelet/bubbles/v2/viewport"
	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/charmbracelet/lipgloss/v2"
	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/deployment"
	"github.com/csnewman/localflux/internal/progress"
	"github.com/csnewman/localflux/internal/relay"
	"golang.org/x/sync/errgroup"
	"os"
	"slices"
	"time"
)

type driverCallbacks interface {
	cluster.Callbacks
	deployment.Callbacks
	relay.Callbacks
}

var (
	spinnerStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	detailStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Margin(0, 2)
	errorDetailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Margin(0, 2)
	durationStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	infoMark         = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).SetString("ℹ")
	checkMark        = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).SetString("✓")
	warnMark         = lipgloss.NewStyle().Foreground(lipgloss.Color("148")).SetString("⚠")
	errorMark        = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).SetString("⚠")
)

func drive(ctx context.Context, fn func(ctx context.Context, cb driverCallbacks) error) error {
	if plainOutput {
		return drivePlain(ctx, fn)
	}

	return driveUI(ctx, fn)
}

func drivePlain(ctx context.Context, fn func(ctx context.Context, cb driverCallbacks) error) error {
	driver := &plainCallbacks{}
	err := fn(ctx, driver)
	driver.exiting(err)
	return err
}

func driveUI(ctx context.Context, fn func(ctx context.Context, cb driverCallbacks) error) error {
	outerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(outerCtx)

	p := tea.NewProgram(newModel(cancel), tea.WithContext(ctx))
	defer p.Quit()

	g.Go(func() error {
		defer cancel()

		_, err := p.Run()

		return err
	})

	g.Go(func() error {
		err := fn(gctx, &uiCallbacks{
			p: p,
		})

		p.Send(&stateData{
			exit:    true,
			exitErr: err,
		})

		return err

	})

	return g.Wait()
}

type model struct {
	spinner   spinner.Model
	cleanExit bool
	dirtyExit bool
	state     *stateData
	width     int
	height    int
	exitFunc  func()
	stepLines []string
	vp        viewport.Model

	trace *progress.Trace
}

func newModel(exitFunc func()) model {
	s := spinner.New()
	s.Style = spinnerStyle

	return model{
		spinner: s,
		state: &stateData{
			msg:    "...",
			detail: "...",
			start:  time.Now(),
		},
		exitFunc: exitFunc,
	}
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			m.exitFunc()
		}

		return m, nil
	case *stateData:
		if msg.exit {
			if msg.exitErr == nil {
				m.cleanExit = true
			} else {
				m.dirtyExit = true
			}

			return m, tea.Quit
		}

		m.state = msg
		return m, nil
	case stepLines:
		m.stepLines = msg.Lines
		return m, nil
	case *deployment.SolveStatus:
		if msg == nil {
			m.trace = nil

			return m, nil
		}

		if m.trace == nil {
			m.trace = progress.NewTrace(true)
		}

		m.trace.Update(msg, m.width-5)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	default:
		return m, nil
	}
}

func (m model) View() string {
	if m.cleanExit {
		return ""
	}

	var s string

	s += m.spinner.View() + " " + m.state.msg + " " + durationStyle.Render(time.Since(m.state.start).Round(time.Second).String())

	if m.state.detail != "" {
		s += "\n" + detailStyle.Width(m.width).Render(m.state.detail)
	}

	if len(m.stepLines) > 0 {
		s += "\n" + detailStyle.Width(m.width).Render("----")

		for _, l := range m.stepLines {
			s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf("> %s", l))
		}
	}

	if m.trace != nil {
		d := m.trace.DisplayInfo()

		d.Jobs = progress.SetupTerminals(d.Jobs, m.height-5, true)

		s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf(
			"[+] Building %.1fs (%d/%d)",
			time.Since(d.StartTime).Seconds(),
			d.CountCompleted,
			d.CountTotal,
		))

		for _, j := range d.Jobs {
			if len(j.Intervals) == 0 {
				continue
			}
			var dt float64
			for _, ival := range j.Intervals {
				dt += ival.Duration().Seconds()
			}
			if dt < 0.05 {
				dt = 0
			}
			pfx := " => "
			timer := fmt.Sprintf(" %3.1fs", dt)
			status := j.Status
			showStatus := false

			left := m.width - len(pfx) - len(timer) - 1
			if status != "" {
				if left+len(status) > 20 {
					showStatus = true
					left -= len(status) + 1
				}
			}
			if left < 12 { // too small screen to show progress
				continue
			}
			name := j.Name
			if len(name) > left {
				name = name[:left]
			}

			out := pfx + name
			if showStatus {
				out += " " + status
			}

			out = align(out, timer, m.width-4)

			s += "\n" + detailStyle.Width(m.width).Render(out)

			if j.ShowTerm {
				term := j.Vertex.Term
				term.Resize(progress.TermHeight, m.width-progress.TermPad)
				for _, l := range term.Content {
					if !isEmpty(l) {
						s += "\n" + detailStyle.Width(m.width).Render(" => => "+string(l))
					}
				}
				j.Vertex.TermCount++
				j.ShowTerm = false
			}
		}

		if m.dirtyExit {
			s += "\n" + errorDetailStyle.Width(m.width).Render(m.trace.ErrorLogs())
		}
	}

	s += "\n"

	return s
}

func isEmpty(l []rune) bool {
	for _, r := range l {
		if r != ' ' {
			return false
		}
	}
	return true
}

func align(l, r string, w int) string {
	return fmt.Sprintf("%-[2]*[1]s %[3]s", l, w-len(r)-1, r)
}

type stateData struct {
	msg     string
	detail  string
	start   time.Time
	exit    bool
	exitErr error
}

type stepLines struct {
	Lines []string
}

type uiCallbacks struct {
	p *tea.Program
}

func (c *uiCallbacks) StepLines(lines []string) {
	c.p.Send(stepLines{Lines: slices.Clone(lines)})
}

func (c *uiCallbacks) BuildStatus(name string, graph *deployment.SolveStatus) {
	c.p.Send(graph)
}

func (c *uiCallbacks) Success(detail string) {
	c.p.Printf("%s %s", checkMark, detail)
}

func (c *uiCallbacks) Info(msg string) {
	c.p.Printf("%s %s", infoMark, msg)
}

func (c *uiCallbacks) Warn(msg string) {
	c.p.Printf("%s %s", warnMark, msg)
}

func (c *uiCallbacks) Error(msg string) {
	c.p.Printf("%s %s", errorMark, msg)
}

func (c *uiCallbacks) Completed(msg string, dur time.Duration) {
	c.p.Printf("%s %s %s", checkMark, msg, durationStyle.Render(dur.Round(time.Second).String()))
}

func (c *uiCallbacks) State(msg string, detail string, start time.Time) {
	c.p.Send(&stateData{
		msg:    msg,
		detail: detail,
		start:  start,
	})
}

type plainCallbacks struct {
	lastMsg    string
	lastDetail string
	lastLines  []string

	trace *progress.Trace
	mux   *progress.TextMux
}

func (c *plainCallbacks) State(msg string, detail string, start time.Time) {
	if c.lastMsg == msg && c.lastDetail == detail {
		return
	}

	c.lastMsg = msg
	c.lastDetail = detail

	if c.lastDetail == "" {
		fmt.Println("step:", msg)
	} else {
		fmt.Println("step:", msg, "-", detail)
	}
}

func (c *plainCallbacks) Success(detail string) {
	fmt.Println("success:", detail)
}

func (c *plainCallbacks) Info(msg string) {
	fmt.Println("info:", msg)
}

func (c *plainCallbacks) Warn(msg string) {
	fmt.Println("info:", msg)
}

func (c *plainCallbacks) Error(msg string) {
	fmt.Println("error:", msg)
}

func (c *plainCallbacks) Completed(msg string, dur time.Duration) {
	fmt.Println("completed:", msg, dur.Round(time.Second))
}

func (c *plainCallbacks) exiting(err error) {
	if err != nil && c.trace != nil {
		fmt.Println(c.trace.ErrorLogs())
	}
}

func (c *plainCallbacks) BuildStatus(name string, graph *deployment.SolveStatus) {
	if graph == nil {
		c.trace = nil
		c.mux = nil

		return
	}

	if c.trace == nil {
		c.trace = progress.NewTrace(false)
		c.mux = progress.NewTextMux(os.Stdout, "Building "+name)
	}

	c.trace.Update(graph, 80)

	c.mux.Print(c.trace)
}

func (c *plainCallbacks) StepLines(lines []string) {
	matches := true

	for i, line := range lines {

		if i >= len(c.lastLines) {
			matches = false
		} else if c.lastLines[i] != line {
			matches = false
		}

		if !matches {
			fmt.Println("progress:", line)
		}
	}

	c.lastLines = slices.Clone(lines)
}
