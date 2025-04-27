package progress

import (
	"container/ring"
	"slices"
	"sort"
	"time"

	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"github.com/tonistiigi/vt100"
)

const (
	termHeightMin = 6
	TermPad       = 10
)

var termHeightInitial = termHeightMin
var TermHeight = termHeightMin

type DisplayInfo struct {
	StartTime      time.Time
	Jobs           []*Job
	CountTotal     int
	CountCompleted int
}

type Job struct {
	Intervals   []Interval
	IsCompleted bool
	Name        string
	Status      string
	HasError    bool
	IsCanceled  bool
	Vertex      *vertex
	ShowTerm    bool
}

type vertex struct {
	*client.Vertex

	statuses []*status
	byID     map[string]*status
	indent   string
	index    int

	logs          [][]byte
	logsPartial   bool
	logsOffset    int
	logsBuffer    *ring.Ring // stores last logs to print them on error
	prev          *client.Vertex
	events        []string
	lastBlockTime *time.Time
	count         int
	statusUpdates map[string]struct{}

	warnings   []client.VertexWarning
	warningIdx int

	jobs      []*Job
	jobCached bool

	Term      *vt100.VT100
	termBytes int
	TermCount int

	// Interval start time in unix nano -> interval. Using a map ensures
	// that updates for the same interval overwrite their previous updates.
	intervals       map[int64]Interval
	mergedIntervals []Interval

	// whether the vertex should be hidden due to being in a progress group
	// that doesn't have any non-weak members that have started
	hidden bool
}

func (v *vertex) update(c int) {
	if v.count == 0 {
		now := time.Now()
		v.lastBlockTime = &now
	}
	v.count += c
}

func (v *vertex) mostRecentInterval() *Interval {
	if v.isStarted() {
		ival := v.mergedIntervals[len(v.mergedIntervals)-1]
		return &ival
	}
	return nil
}

func (v *vertex) isStarted() bool {
	return len(v.mergedIntervals) > 0
}

func (v *vertex) isCompleted() bool {
	if ival := v.mostRecentInterval(); ival != nil {
		return ival.stop != nil
	}
	return false
}

type vertexGroup struct {
	*vertex
	subVtxs map[digest.Digest]client.Vertex
}

func (vg *vertexGroup) refresh() (changed, newlyStarted, newlyRevealed bool) {
	newVtx := *vg.Vertex
	newVtx.Cached = true
	alreadyStarted := vg.isStarted()
	wasHidden := vg.hidden
	for _, subVtx := range vg.subVtxs {
		if subVtx.Started != nil {
			newInterval := Interval{
				start: subVtx.Started,
				stop:  subVtx.Completed,
			}
			prevInterval := vg.intervals[subVtx.Started.UnixNano()]
			if !newInterval.isEqual(prevInterval) {
				changed = true
			}
			if !alreadyStarted {
				newlyStarted = true
			}
			vg.intervals[subVtx.Started.UnixNano()] = newInterval

			if !subVtx.ProgressGroup.Weak {
				vg.hidden = false
			}
		}

		// Group is considered cached iff all subvtxs are cached
		newVtx.Cached = newVtx.Cached && subVtx.Cached

		// Group error is set to the first error found in subvtxs, if any
		if newVtx.Error == "" {
			newVtx.Error = subVtx.Error
		} else {
			vg.hidden = false
		}
	}

	if vg.Cached != newVtx.Cached {
		changed = true
	}
	if vg.Error != newVtx.Error {
		changed = true
	}
	vg.Vertex = &newVtx

	if !vg.hidden && wasHidden {
		changed = true
		newlyRevealed = true
	}

	var ivals []Interval
	for _, ival := range vg.intervals {
		ivals = append(ivals, ival)
	}
	vg.mergedIntervals = mergeIntervals(ivals)

	return changed, newlyStarted, newlyRevealed
}

type status struct {
	*client.VertexStatus
}

func addTime(tm *time.Time, d time.Duration) *time.Time {
	if tm == nil {
		return nil
	}
	t := tm.Add(d)
	return &t
}

func SetupTerminals(jobs []*Job, height int, all bool) []*Job {
	var candidates []*Job
	numInUse := 0
	for _, j := range jobs {
		if j.Vertex != nil && j.Vertex.termBytes > 0 && !j.IsCompleted {
			candidates = append(candidates, j)
		}
		if !j.IsCompleted {
			numInUse++
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		idxI := candidates[i].Vertex.termBytes + candidates[i].Vertex.TermCount*50
		idxJ := candidates[j].Vertex.termBytes + candidates[j].Vertex.TermCount*50
		return idxI > idxJ
	})

	numFree := height - 2 - numInUse
	numToHide := 0
	TermHeight = max(termHeightMin, min(termHeightInitial, height-termHeightMin-1))
	termLimit := TermHeight + 3

	for i := 0; numFree > termLimit && i < len(candidates); i++ {
		candidates[i].ShowTerm = true
		numToHide += candidates[i].Vertex.Term.UsedHeight()
		numFree -= termLimit
	}

	if !all {
		jobs = wrapHeight(jobs, height-2-numToHide)
	}

	return jobs
}

func wrapHeight(j []*Job, limit int) []*Job {
	if limit < 0 {
		return nil
	}
	var wrapped []*Job
	wrapped = append(wrapped, j...)
	if len(j) > limit {
		wrapped = wrapped[len(j)-limit:]

		// wrap things around if incomplete jobs were cut
		var invisible []*Job
		for _, j := range j[:len(j)-limit] {
			if !j.IsCompleted {
				invisible = append(invisible, j)
			}
		}

		if l := len(invisible); l > 0 {
			rewrapped := make([]*Job, 0, len(wrapped))
			for _, j := range wrapped {
				if !j.IsCompleted || l <= 0 {
					rewrapped = append(rewrapped, j)
				}
				l--
			}
			freespace := len(wrapped) - len(rewrapped)
			wrapped = append(invisible[len(invisible)-freespace:], rewrapped...)
		}
	}
	return wrapped
}

type Interval struct {
	start *time.Time
	stop  *time.Time
}

func (ival Interval) Duration() time.Duration {
	if ival.start == nil {
		return 0
	}
	if ival.stop == nil {
		return time.Since(*ival.start)
	}
	return ival.stop.Sub(*ival.start)
}

func (ival Interval) isEqual(other Interval) (isEqual bool) {
	return equalTimes(ival.start, other.start) && equalTimes(ival.stop, other.stop)
}

func equalTimes(t1, t2 *time.Time) bool {
	if t2 == nil {
		return t1 == nil
	}
	if t1 == nil {
		return false
	}
	return t1.Equal(*t2)
}

// mergeIntervals takes a slice of (start, stop) pairs and returns a slice where
// any intervals that overlap in time are combined into a single interval. If an
// interval's stop time is nil, it is treated as positive infinity and consumes
// any intervals after it. Intervals with nil start times are ignored and not
// returned.
func mergeIntervals(intervals []Interval) []Interval {
	// remove any intervals that have not started
	var filtered []Interval
	for _, interval := range intervals {
		if interval.start != nil {
			filtered = append(filtered, interval)
		}
	}
	intervals = filtered

	if len(intervals) == 0 {
		return nil
	}

	// sort intervals by start time
	slices.SortFunc(intervals, func(a, b Interval) int {
		return a.start.Compare(*b.start)
	})

	var merged []Interval
	cur := intervals[0]
	for i := 1; i < len(intervals); i++ {
		next := intervals[i]
		if cur.stop == nil {
			// if cur doesn't stop, all intervals after it will be merged into it
			merged = append(merged, cur)
			return merged
		}
		if cur.stop.Before(*next.start) {
			// if cur stops before next starts, no intervals after cur will be
			// merged into it; cur stands on its own
			merged = append(merged, cur)
			cur = next
			continue
		}
		if next.stop == nil {
			// cur and next partially overlap, but next also never stops, so all
			// subsequent intervals will be merged with both cur and next
			merged = append(merged, Interval{
				start: cur.start,
				stop:  nil,
			})
			return merged
		}
		if cur.stop.After(*next.stop) || cur.stop.Equal(*next.stop) {
			// cur fully subsumes next
			continue
		}
		// cur partially overlaps with next, merge them together into cur
		cur = Interval{
			start: cur.start,
			stop:  next.stop,
		}
	}
	// append anything we are left with
	merged = append(merged, cur)
	return merged
}
