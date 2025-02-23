package main

import (
	"context"
	"fmt"
	"github.com/charmbracelet/bubbles/v2/spinner"
	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/charmbracelet/lipgloss/v2"
	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/deployment"
	"github.com/csnewman/localflux/internal/relay"
	"golang.org/x/sync/errgroup"
	"strings"
	"time"
)

type driverCallbacks interface {
	cluster.Callbacks
	deployment.Callbacks
	relay.Callbacks
}

var (
	spinnerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	detailStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Margin(0, 2)
	durationStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	infoMark      = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).SetString("ℹ")
	checkMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).SetString("✓")
	warnMark      = lipgloss.NewStyle().Foreground(lipgloss.Color("148")).SetString("⚠")
	errorMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).SetString("⚠")
)

func drive(ctx context.Context, fn func(ctx context.Context, cb driverCallbacks) error) error {
	if plainOutput {
		return drivePlain(ctx, fn)
	}

	return driveUI(ctx, fn)
}

func drivePlain(ctx context.Context, fn func(ctx context.Context, cb driverCallbacks) error) error {
	return fn(ctx, &plainCallbacks{})
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
	spinner    spinner.Model
	cleanExit  bool
	state      *stateData
	width      int
	buildGraph *deployment.BuildGraph
	exitFunc   func()
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
			}

			return m, tea.Quit
		}

		m.state = msg
		return m, nil
	case *deployment.BuildGraph:
		m.buildGraph = msg
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

	if m.buildGraph != nil {
		m.buildGraph.Mu.Lock()
		defer m.buildGraph.Mu.Unlock()

		s += "\n" + detailStyle.Width(m.width).Render("----")

		for _, id := range m.buildGraph.NodeOrder {
			n := m.buildGraph.Nodes[id]

			completed := time.Now()

			if n.Completed != nil {
				completed = *n.Completed
			}

			dur := completed.Sub(*n.Started).Round(time.Second)

			s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf("%s %s %s", n.Name, dur.String(), n.Error))

			for _, sid := range n.StatusOrder {
				st := n.Statuses[sid]

				completed := time.Now()

				if st.Completed != nil {
					completed = *st.Completed
				}

				dur := completed.Sub(*st.Started).Round(time.Second)

				var v string

				if st.Current != 0 || st.Total != 0 {
					v = fmt.Sprintf("[%d/%d]", st.Current, st.Total)
				}

				s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf("| %s %s %s", sid, dur.String(), v))
			}

			if len(n.Logs) > 0 {
				for l := range strings.Lines(string(n.Logs)) {
					s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf("> %s", l))
				}
			}

			for _, warning := range n.Warnings {
				s += "\n" + detailStyle.Width(m.width).Render(fmt.Sprintf("! %s", string(warning.Short)))
			}
		}
	}

	s += "\n"

	return s
}

type stateData struct {
	msg     string
	detail  string
	start   time.Time
	exit    bool
	exitErr error
}

type uiCallbacks struct {
	p *tea.Program
}

func (c *uiCallbacks) BuildStatus(name string, graph *deployment.BuildGraph) {
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
	lastGraph  time.Time
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

func (c *plainCallbacks) BuildStatus(name string, graph *deployment.BuildGraph) {
	if graph == nil {
		return
	}

	graph.Mu.Lock()
	defer graph.Mu.Unlock()

	if graph.Final {
		c.lastGraph = time.Time{}
	} else if time.Since(c.lastGraph) < time.Second*15 {
		return
	} else {
		c.lastGraph = time.Now()
	}

	fmt.Println("----")

	for _, id := range graph.NodeOrder {
		n := graph.Nodes[id]

		fmt.Println(n.Name, n.Started, n.Completed, n.Error)

		for _, sid := range n.StatusOrder {
			s := n.Statuses[sid]

			fmt.Println("  |", sid, s.Started, s.Completed, s.Current, s.Total)
		}

		for _, log := range n.Logs {
			fmt.Println("  >", string(log))
		}

		for _, warning := range n.Warnings {
			fmt.Println("  !", string(warning.Short))
		}
	}
}
