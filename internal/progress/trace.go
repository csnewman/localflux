// Package progress is based on https://github.com/moby/buildkit/tree/master/util/progress/progressui, Licensed under
// Apache License Version 2.0, January 2004
package progress

import (
	"bytes"
	"context"
	"fmt"
	"github.com/moby/buildkit/client"
	"github.com/opencontainers/go-digest"
	"github.com/tonistiigi/units"
	"github.com/tonistiigi/vt100"
	"strings"
	"time"
)

type Trace struct {
	startTime     *time.Time
	localTimeDiff time.Duration
	vertexes      []*vertex
	byDigest      map[digest.Digest]*vertex
	updates       map[digest.Digest]struct{}
	modeConsole   bool
	groups        map[string]*vertexGroup // group id -> group
}

func NewTrace(modeConsole bool) *Trace {
	return &Trace{
		byDigest:    make(map[digest.Digest]*vertex),
		updates:     make(map[digest.Digest]struct{}),
		modeConsole: modeConsole,
		groups:      make(map[string]*vertexGroup),
	}
}

func (t *Trace) triggerVertexEvent(v *client.Vertex) {
	if v.Started == nil {
		return
	}

	var old client.Vertex
	vtx := t.byDigest[v.Digest]
	if v := vtx.prev; v != nil {
		old = *v
	}

	changed := false
	if v.Digest != old.Digest {
		changed = true
	}
	if v.Name != old.Name {
		changed = true
	}
	if v.Started != old.Started {
		if v.Started != nil && old.Started == nil || !v.Started.Equal(*old.Started) {
			changed = true
		}
	}
	if v.Completed != old.Completed && v.Completed != nil {
		changed = true
	}
	if v.Cached != old.Cached {
		changed = true
	}
	if v.Error != old.Error {
		changed = true
	}

	if changed {
		vtx.update(1)
		t.updates[v.Digest] = struct{}{}
	}

	t.byDigest[v.Digest].prev = v
}

func (t *Trace) Update(s *client.SolveStatus, termWidth int) {
	seenGroups := make(map[string]struct{})
	var groups []string
	for _, v := range s.Vertexes {
		if t.startTime == nil {
			t.startTime = v.Started
		}
		if v.ProgressGroup != nil {
			group, ok := t.groups[v.ProgressGroup.Id]
			if !ok {
				group = &vertexGroup{
					vertex: &vertex{
						Vertex: &client.Vertex{
							Digest: digest.Digest(v.ProgressGroup.Id),
							Name:   v.ProgressGroup.Name,
						},
						byID:          make(map[string]*status),
						statusUpdates: make(map[string]struct{}),
						intervals:     make(map[int64]Interval),
						hidden:        true,
					},
					subVtxs: make(map[digest.Digest]client.Vertex),
				}
				if t.modeConsole {
					group.Term = vt100.NewVT100(TermHeight, termWidth-TermPad)
				}
				t.groups[v.ProgressGroup.Id] = group
				t.byDigest[group.Digest] = group.vertex
			}
			if _, ok := seenGroups[v.ProgressGroup.Id]; !ok {
				groups = append(groups, v.ProgressGroup.Id)
				seenGroups[v.ProgressGroup.Id] = struct{}{}
			}
			group.subVtxs[v.Digest] = *v
			t.byDigest[v.Digest] = group.vertex
			continue
		}
		prev, ok := t.byDigest[v.Digest]
		if !ok {
			t.byDigest[v.Digest] = &vertex{
				byID:          make(map[string]*status),
				statusUpdates: make(map[string]struct{}),
				intervals:     make(map[int64]Interval),
			}
			if t.modeConsole {
				t.byDigest[v.Digest].Term = vt100.NewVT100(TermHeight, termWidth-TermPad)
			}
		}
		t.triggerVertexEvent(v)
		if v.Started != nil && (prev == nil || !prev.isStarted()) {
			if t.localTimeDiff == 0 {
				t.localTimeDiff = time.Since(*v.Started)
			}
			t.vertexes = append(t.vertexes, t.byDigest[v.Digest])
		}
		// allow a duplicate initial vertex that shouldn't reset state
		if !(prev != nil && prev.isStarted() && v.Started == nil) {
			t.byDigest[v.Digest].Vertex = v
		}
		if v.Started != nil {
			t.byDigest[v.Digest].intervals[v.Started.UnixNano()] = Interval{
				start: v.Started,
				stop:  v.Completed,
			}
			var ivals []Interval
			for _, ival := range t.byDigest[v.Digest].intervals {
				ivals = append(ivals, ival)
			}
			t.byDigest[v.Digest].mergedIntervals = mergeIntervals(ivals)
		}
		t.byDigest[v.Digest].jobCached = false
	}
	for _, groupID := range groups {
		group := t.groups[groupID]
		changed, newlyStarted, newlyRevealed := group.refresh()
		if newlyStarted {
			if t.localTimeDiff == 0 {
				t.localTimeDiff = time.Since(*group.mergedIntervals[0].start)
			}
		}
		if group.hidden {
			continue
		}
		if newlyRevealed {
			t.vertexes = append(t.vertexes, group.vertex)
		}
		if changed {
			group.update(1)
			t.updates[group.Digest] = struct{}{}
		}
		group.jobCached = false
	}
	for _, s := range s.Statuses {
		v, ok := t.byDigest[s.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		prev, ok := v.byID[s.ID]
		if !ok {
			v.byID[s.ID] = &status{VertexStatus: s}
		}
		if s.Started != nil && (prev == nil || prev.Started == nil) {
			v.statuses = append(v.statuses, v.byID[s.ID])
		}
		v.byID[s.ID].VertexStatus = s
		v.statusUpdates[s.ID] = struct{}{}
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}
	for _, w := range s.Warnings {
		v, ok := t.byDigest[w.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.warnings = append(v.warnings, *w)
		v.update(1)
	}
	for _, l := range s.Logs {
		v, ok := t.byDigest[l.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		if v.Term != nil {
			if v.Term.Width != termWidth {
				TermHeight = max(termHeightMin, min(termHeightInitial, v.Term.Height-termHeightMin-1))
				v.Term.Resize(TermHeight, termWidth-TermPad)
			}
			v.termBytes += len(l.Data)
			v.Term.Write(l.Data) // error unhandled on purpose. don't trust vt100
		}
		i := 0
		complete := split(l.Data, byte('\n'), func(dt []byte) {
			if v.logsPartial && len(v.logs) != 0 && i == 0 {
				v.logs[len(v.logs)-1] = append(v.logs[len(v.logs)-1], dt...)
			} else {
				ts := time.Duration(0)
				if ival := v.mostRecentInterval(); ival != nil {
					ts = l.Timestamp.Sub(*ival.start)
				}
				prec := 1
				sec := ts.Seconds()
				if sec < 10 {
					prec = 3
				} else if sec < 100 {
					prec = 2
				}
				v.logs = append(v.logs, fmt.Appendf(nil, "%s %s", fmt.Sprintf("%.[2]*[1]f", sec, prec), dt))
			}
			i++
		})
		v.logsPartial = !complete
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}
}

func (t *Trace) ErrorLogs() string {
	f := &strings.Builder{}

	for _, v := range t.vertexes {
		if v.Error != "" && !strings.HasSuffix(v.Error, context.Canceled.Error()) {
			fmt.Fprintln(f, "------")
			fmt.Fprintf(f, " > %s:\n", v.Name)
			// tty keeps original logs
			for _, l := range v.logs {
				f.Write(l)
				fmt.Fprintln(f)
			}
			// printer keeps last logs buffer
			if v.logsBuffer != nil {
				for range v.logsBuffer.Len() {
					if v.logsBuffer.Value != nil {
						fmt.Fprintln(f, string(v.logsBuffer.Value.([]byte)))
					}
					v.logsBuffer = v.logsBuffer.Next()
				}
			}
			fmt.Fprintln(f, "------")
		}
	}

	return f.String()
}

func (t *Trace) DisplayInfo() (d DisplayInfo) {
	d.StartTime = time.Now()
	if t.startTime != nil {
		d.StartTime = t.startTime.Add(t.localTimeDiff)
	}
	d.CountTotal = len(t.byDigest)
	for _, v := range t.byDigest {
		if v.ProgressGroup != nil || v.hidden {
			// don't count vtxs in a group, they are merged into a single vtx
			d.CountTotal--
			continue
		}
		if v.isCompleted() {
			d.CountCompleted++
		}
	}

	for _, v := range t.vertexes {
		if v.jobCached {
			d.Jobs = append(d.Jobs, v.jobs...)
			continue
		}
		var jobs []*Job
		j := &Job{
			Name:        strings.ReplaceAll(v.Name, "\t", " "),
			Vertex:      v,
			IsCompleted: true,
		}
		for _, ival := range v.intervals {
			j.Intervals = append(j.Intervals, Interval{
				start: addTime(ival.start, t.localTimeDiff),
				stop:  addTime(ival.stop, t.localTimeDiff),
			})
			if ival.stop == nil {
				j.IsCompleted = false
			}
		}
		j.Intervals = mergeIntervals(j.Intervals)
		if v.Error != "" {
			if strings.HasSuffix(v.Error, context.Canceled.Error()) {
				j.IsCanceled = true
				j.Name = "CANCELED " + j.Name
			} else {
				j.HasError = true
				j.Name = "ERROR " + j.Name
			}
		}
		if v.Cached {
			j.Name = "CACHED " + j.Name
		}
		j.Name = v.indent + j.Name
		jobs = append(jobs, j)
		for _, s := range v.statuses {
			j := &Job{
				Intervals: []Interval{{
					start: addTime(s.Started, t.localTimeDiff),
					stop:  addTime(s.Completed, t.localTimeDiff),
				}},
				IsCompleted: s.Completed != nil,
				Name:        v.indent + "=> " + s.ID,
			}
			if s.Total != 0 {
				j.Status = fmt.Sprintf("%.2f / %.2f", units.Bytes(s.Current), units.Bytes(s.Total))
			} else if s.Current != 0 {
				j.Status = fmt.Sprintf("%.2f", units.Bytes(s.Current))
			}
			jobs = append(jobs, j)
		}
		for _, w := range v.warnings {
			msg := "WARN: " + string(w.Short)
			var mostRecentInterval Interval
			if ival := v.mostRecentInterval(); ival != nil {
				mostRecentInterval = *ival
			}
			j := &Job{
				Intervals: []Interval{{
					start: addTime(mostRecentInterval.start, t.localTimeDiff),
					stop:  addTime(mostRecentInterval.stop, t.localTimeDiff),
				}},
				Name:       msg,
				IsCanceled: true,
			}
			jobs = append(jobs, j)
		}
		d.Jobs = append(d.Jobs, jobs...)
		v.jobs = jobs
		v.jobCached = true
	}

	return d
}

func split(dt []byte, sep byte, fn func([]byte)) bool {
	if len(dt) == 0 {
		return false
	}
	for {
		if len(dt) == 0 {
			return true
		}
		idx := bytes.IndexByte(dt, sep)
		if idx == -1 {
			fn(dt)
			return false
		}
		fn(dt[:idx])
		dt = dt[idx+1:]
	}
}
