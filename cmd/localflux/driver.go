package main

import (
	"context"
	"fmt"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/csnewman/localflux/internal/cluster"
	"golang.org/x/sync/errgroup"
	"time"
)

var (
	spinnerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	detailStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Margin(0, 2)
	durationStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	infoMark      = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).SetString("ℹ")
	checkMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).SetString("✓")
	warnMark      = lipgloss.NewStyle().Foreground(lipgloss.Color("148")).SetString("⚠")
	errorMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).SetString("⚠")
)

func drive(ctx context.Context, fn func(ctx context.Context, cb cluster.Callbacks) error) error {
	if plainOutput {
		return drivePlain(ctx, fn)
	}

	return driveUI(ctx, fn)
}

func drivePlain(ctx context.Context, fn func(ctx context.Context, cb cluster.Callbacks) error) error {
	return fn(ctx, &plainCallbacks{})
}

func driveUI(ctx context.Context, fn func(ctx context.Context, cb cluster.Callbacks) error) error {
	outerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(outerCtx)

	p := tea.NewProgram(newModel(), tea.WithContext(gctx))
	defer p.Quit()

	g.Go(func() error {
		defer cancel()

		_, err := p.Run()

		return err
	})

	g.Go(func() error {
		defer p.Send(&stateData{
			exit: true,
		})

		return fn(gctx, &uiCallbacks{
			p: p,
		})
	})

	return g.Wait()
}

type model struct {
	spinner  spinner.Model
	quitting bool
	state    *stateData
	width    int
}

func newModel() model {
	s := spinner.New()
	s.Style = spinnerStyle
	return model{
		spinner: s,
		state: &stateData{
			msg:    "...",
			detail: "...",
			start:  time.Now(),
		},
	}
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.quitting = true
			return m, tea.Quit
		}

		return m, nil

	case *stateData:
		if msg.exit {
			m.quitting = true
			return m, tea.Quit
		}

		m.state = msg
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
	var s string

	if m.quitting {
		return ""
	} else {
		s += m.spinner.View() + " " + m.state.msg + " " + durationStyle.Render(time.Since(m.state.start).Round(time.Second).String())

		if m.state.detail != "" {
			s += "\n" + detailStyle.Width(m.width).Render(m.state.detail)
		}

		s += "\n"
	}

	return s
}

type stateData struct {
	msg    string
	detail string
	start  time.Time
	exit   bool
}

type uiCallbacks struct {
	p *tea.Program
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
